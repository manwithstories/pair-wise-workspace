package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"bcastqc/internal/ebml"
	"bcastqc/internal/fixture"
	"bcastqc/internal/jitter"
	"bcastqc/internal/media"
	"bcastqc/internal/mux"
	"bcastqc/internal/netem"
	rtpx "bcastqc/internal/rtp"
	avsync "bcastqc/internal/sync"
)

// 阶段间有界事件通道的报文条数上限（与缓存字节上限双重约束）。
const stageChannelCap = 64

// Run 处理一整个批次（1~32 个样例），返回与输入等长、等序的结果。
// 单文件失败只记录在对应 Result 中，不影响其他样例。
func Run(ctx context.Context, specs []fixture.Spec) ([]Result, error) {
	if len(specs) < 1 || len(specs) > 32 {
		return nil, media.NewError(media.StageCLI, media.KindArg,
			"批次大小必须在 1~32 之间")
	}
	results := make([]Result, len(specs))
	sem := make(chan struct{}, 4)
	done := make(chan int, len(specs))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i, spec := range specs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return results, ctx.Err()
		}
		go func(idx int, s fixture.Spec) {
			defer func() { <-sem }()
			results[idx] = runOne(ctx, s)
			done <- idx
		}(i, spec)
	}
	for range specs {
		select {
		case <-done:
		case <-ctx.Done():
			return results, ctx.Err()
		}
	}
	return results, nil
}

// runOne 执行单样例全流程；任何阶段失败都不会写出半成品。
func runOne(ctx context.Context, s fixture.Spec) Result {
	res := Result{SpecName: s.Name}
	m := Metrics{Case: s.Name, FirstMissingOff: -1}

	raw, err := fixture.Generate(s)
	if err != nil {
		return reject(m, err)
	}

	// 阶段 1：EBML 解读。结构损坏立即终止，不产出文件。
	doc, err := ebml.ParseDocument(ebml.NewBytesReader(raw))
	if err != nil {
		m.FirstMissingOff = firstMissingOffset(err)
		m.Rejected = true
		m.ErrKind = string(media.AsError(err, media.StageParse, media.KindTruncated).Kind)
		m.ErrDetail = err.Error()
		res.Metrics = m
		res.Err = err
		return res
	}

	out, err := streamThrough(ctx, s, doc, &m)
	if err != nil {
		me := media.AsError(err, media.StageRecover, media.KindGap)
		m.FirstMissingOff = firstMissingOffset(err)
		m.ErrKind = string(me.Kind)
		m.ErrDetail = err.Error()
		m.Recoverable = me.Recoverable
		if me.Kind == media.KindCanceled || me.Kind == media.KindTruncated ||
			me.Kind == media.KindBoundary {
			m.Rejected = true
		}
		res.Metrics = m
		res.Err = err
		return res
	}

	sum := sha256.Sum256(out)
	m.OutputSHA256 = hex.EncodeToString(sum[:])
	res.Metrics = m
	res.Output = out
	return res
}

// streamThrough 用生产/消费并发串联 RTP 打包、损伤传输与有界抖动缓存，
// 缓存满载时 Push 阻塞，背压经 link 反传到生产者，逐级传导。
func streamThrough(ctx context.Context, s fixture.Spec, doc *ebml.Document,
	m *Metrics) ([]byte, error) {
	link := netem.NewLink(s.Link)
	videoPz := rtpx.NewPacketizer(media.TrackVideo, media.VideoClockRate, 1000, rtpx.PayloadTypeVideo)
	audioPz := rtpx.NewPacketizer(media.TrackAudio, media.AudioClockRate, 1001, rtpx.PayloadTypeAudio)
	rcvVideo := jitter.NewReceiver(media.TrackVideo, s.QueueBytes, s.CacheBytes, s.Window, ctx.Done())
	rcvAudio := jitter.NewReceiver(media.TrackAudio, s.QueueBytes, s.CacheBytes, s.Window, ctx.Done())
	defer rcvVideo.Close()
	defer rcvAudio.Close()

	// 生产者：打包 -> 重传缓存 -> 链路损伤 -> 交付事件通道（有界）。
	events := make(chan netem.Event, stageChannelCap)
	prodErr := make(chan error, 1)
	go func() {
		prodErr <- produce(ctx, doc, link, videoPz, audioPz, rcvVideo, rcvAudio, events)
	}()

	// 单消费者：每入队一个报文前先排空一轮，顺序确定、峰值可复现，
	// 满载时通过有界 events 通道把背压传回生产者，且不会死锁。
	if err := consume(ctx, events, rcvVideo, rcvAudio); err != nil {
		return nil, err
	}
	if err := <-prodErr; err != nil {
		return nil, err
	}

	// 阶段 4：恢复与帧重组，喂入同步时间线。
	aligner := avsync.New()
	var recoveredBlocks []mux.MuxBlock
	if err := recoverAndReassemble(ctx, rcvVideo, rcvAudio, doc,
		aligner, &recoveredBlocks, m); err != nil {
		return nil, err
	}

	// 阶段 5：同步统计（不静默修正回退）。
	rep := aligner.Compute()
	m.SyncMaxMS = rep.MaxDiffMS()
	if rep.Negatives > 0 {
		return nil, media.NewError(media.StageSync, media.KindTimestamp, "检测到负时间戳")
	}

	fillTransportMetrics(m, rcvVideo, rcvAudio)

	// 阶段 6：仅在全部前置阶段成功后再封装。
	tracks := []mux.TrackSpec{
		{Number: media.TrackAudio, Type: ebml.TrackTypeAudio},
		{Number: media.TrackVideo, Type: ebml.TrackTypeVideo},
	}
	out, err := mux.Write(tracks, recoveredBlocks)
	if err != nil {
		return nil, media.NewError(media.StageMux, media.KindIO, "再封装失败").Wrap(err)
	}
	return out, nil
}

// produce 顺序打包所有块并把存活报文推入事件通道；
// 通道满时阻塞，从而把下游缓存背压传回打包阶段。
func produce(ctx context.Context, doc *ebml.Document, link *netem.Link,
	videoPz, audioPz *rtpx.Packetizer,
	rcvVideo, rcvAudio *jitter.Receiver, events chan<- netem.Event) error {
	defer close(events)
	var sendClock int64
	// 由于重排在 link 内部交换相邻位置，这里按发送批即时排空链路，
	// 既保留乱序/重复语义，又让事件流持续供给消费者。
	for _, blk := range doc.Blocks {
		if ctx.Err() != nil {
			return media.NewError(media.StageRecover, media.KindCanceled, "管线被取消")
		}
		f := rtpx.Frame{Track: blk.Track, TimeNS: blk.TimeNS, Key: blk.Keyframe, Payload: blk.Payload}
		pz := videoPz
		if blk.Track == media.TrackAudio {
			pz = audioPz
		}
		pkts, err := pz.Packetize(f, sendClock)
		if err != nil {
			return err
		}
		for _, p := range pkts {
			sendClock += 1_000_000
			p.SendNS = sendClock
			cacheFor(rcvVideo, rcvAudio, p.Track).Store(p)
			if link.Accept(p, sendClock) {
				// 取出该报文产生的所有事件（可能含重复）。
				for _, ev := range link.Drain() {
					select {
					case events <- ev:
					case <-ctx.Done():
						return media.NewError(media.StageRecover, media.KindCanceled, "管线被取消")
					}
				}
			}
		}
	}
	return nil
}

func cacheFor(v, a *jitter.Receiver, track uint32) *jitter.RetransmitCache {
	if track == media.TrackAudio {
		return a.Cache()
	}
	return v.Cache()
}

func receiverFor(v, a *jitter.Receiver, track uint32) *jitter.Receiver {
	if track == media.TrackAudio {
		return a
	}
	return v
}

// consume 在单个 goroutine 内驱动有界缓存：每接收一个事件先 Pump 排空，
// 再 Handle 入队。处理顺序确定，故峰值字节与统计在同种子下可复现；
// 缓存满载时 Handle 阻塞，生产者侧的有界 events 通道随之停止接收，
// 背压沿 链路 -> 通道 -> 打包阶段 逐级传导。
func consume(ctx context.Context, events <-chan netem.Event,
	rcvVideo, rcvAudio *jitter.Receiver) error {
	for ev := range events {
		if ctx.Err() != nil {
			return media.NewError(media.StageRecover, media.KindCanceled, "管线被取消")
		}
		// 先排空两条轨道，为即将入队的报文腾出有界空间。
		if _, err := rcvVideo.Pump(ctx); err != nil {
			return err
		}
		if _, err := rcvAudio.Pump(ctx); err != nil {
			return err
		}
		rcv := receiverFor(rcvVideo, rcvAudio, ev.Packet.Track)
		if err := rcv.Handle(ctx, ev.Packet, ev.ArriveNS); err != nil {
			return err
		}
	}
	// 发送流结束：封口后多轮 Pump 完成最终重传并把缓存抽干。
	rcvVideo.Seal()
	rcvAudio.Seal()
	for {
		vGot, err := rcvVideo.Pump(ctx)
		if err != nil {
			return err
		}
		aGot, err := rcvAudio.Pump(ctx)
		if err != nil {
			return err
		}
		if !vGot && !aGot {
			break
		}
	}
	return nil
}

// reject 把样例生成阶段的错误规整为拒绝结果。
func reject(m Metrics, err error) Result {
	me := media.AsError(err, media.StageCLI, media.KindIO)
	m.Rejected = true
	m.ErrKind = string(me.Kind)
	m.ErrDetail = err.Error()
	return Result{Metrics: m, Err: err}
}
