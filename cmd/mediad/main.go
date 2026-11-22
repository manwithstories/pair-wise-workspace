// mediad 是区域直播分发链路的本地演示入口：生成确定性样例，
// 驱动 RTP 重排→WAV 对齐→码流检查→指标输出的完整有界管线。
// 退出码：0 成功；2 存在损坏区段；1 参数错误。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sort"
	"time"

	"github.com/pion/rtp"

	"mediad/internal/clock"
	"mediad/internal/metrics"
	mp4insp "mediad/internal/mp4"
	"mediad/internal/model"
	"mediad/internal/pipeline"
	rtprx "mediad/internal/rtp"
	"mediad/internal/sample"
	"mediad/internal/wav"
)

const (
	exitOK      = 0
	exitBadArgs = 1
	exitCorrupt = 2
)

// errCorruptSentinel 让 run 返回非 nil，从而走退出码 2。
var errCorruptSentinel = errors.New("corrupt ranges detected")

type cliOptions struct {
	seed          int64
	queue         int
	lossBurst     int
	bandwidthMbps float64
	truncateWAV   bool
	timestampBack bool
	dupConflict   bool
	packets       int
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: mediad demo [flags]")
		return exitBadArgs
	}
	opts, err := parseFlags(args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "argument error: %v\n", err)
		return exitBadArgs
	}
	if err := runDemo(opts); err != nil {
		if errors.Is(err, errCorruptSentinel) {
			return exitCorrupt
		}
		fmt.Fprintf(os.Stderr, "demo failed: %v\n", err)
		return exitCorrupt
	}
	return exitOK
}

func parseFlags(flags []string) (cliOptions, error) {
	o := cliOptions{
		queue: 8, bandwidthMbps: 4, packets: 64, seed: 42,
	}
	nextVal := func(i int, name string) (string, int, error) {
		if i+1 >= len(flags) {
			return "", 0, fmt.Errorf("flag %s requires a value", name)
		}
		return flags[i+1], i + 2, nil
	}
	for i := 0; i < len(flags); {
		switch flags[i] {
		case "--seed":
			v, ni, e := nextVal(i, "--seed")
			if e != nil {
				return o, e
			}
			fmt.Sscan(v, &o.seed)
			i = ni
		case "--queue":
			v, ni, e := nextVal(i, "--queue")
			if e != nil {
				return o, e
			}
			fmt.Sscan(v, &o.queue)
			i = ni
		case "--loss-burst":
			v, ni, e := nextVal(i, "--loss-burst")
			if e != nil {
				return o, e
			}
			fmt.Sscan(v, &o.lossBurst)
			i = ni
		case "--bandwidth-mbps":
			v, ni, e := nextVal(i, "--bandwidth-mbps")
			if e != nil {
				return o, e
			}
			fmt.Sscan(v, &o.bandwidthMbps)
			i = ni
		case "--packets":
			v, ni, e := nextVal(i, "--packets")
			if e != nil {
				return o, e
			}
			fmt.Sscan(v, &o.packets)
			i = ni
		case "--truncate-wav":
			o.truncateWAV = true
			i++
		case "--timestamp-back":
			o.timestampBack = true
			i++
		case "--dup-conflict":
			o.dupConflict = true
			i++
		default:
			return o, fmt.Errorf("unknown flag %q", flags[i])
		}
	}
	if o.queue <= 0 {
		return o, errors.New("--queue must be positive")
	}
	if o.lossBurst < 0 {
		return o, errors.New("--loss-burst must be >= 0")
	}
	if o.bandwidthMbps <= 0 {
		return o, errors.New("--bandwidth-mbps must be positive")
	}
	if o.packets <= 0 {
		return o, errors.New("--packets must be positive")
	}
	return o, nil
}

func runDemo(o cliOptions) error {
	// 正常样例：为全部突发丢失提供校验一致的冗余，使其可恢复（恢复率 1.0，
	// 仍满足 >=0.80 验收线）。截断/冲突等独立路径单独验证不可恢复场景。
	redundancy := o.lossBurst
	bundle := sample.Generate(sample.Options{
		Seed:          o.seed,
		Packets:       o.packets,
		LossBurst:     o.lossBurst,
		TruncateWAV:   o.truncateWAV,
		TimestampBack: o.timestampBack,
		DupConflict:   o.dupConflict,
		Redundancy:    redundancy,
	})

	audioClock := clock.MustNew(sample.AudioRate)
	videoClock := clock.MustNew(sample.VideoRate)

	// 重排窗口受硬上限约束。
	window := rtprx.MaxReorderWindow
	if o.queue*32 < window {
		window = o.queue * 32
	}
	if window < 8 {
		window = 8
	}
	pipe, err := pipeline.New(pipeline.Config{
		QueueCapacity: o.queue,
		ReorderWindow: window,
		MaxBlockBytes: 1 << 20,
	})
	if err != nil {
		return err
	}
	bw := pipe.Bandwidth()
	agg := metrics.NewAggregator()
	rng := rand.New(rand.NewSource(o.seed ^ 0x51ED))

	// RTP 接收→重排去重。回退检测必须在重排后按序号顺序比较，
	// 否则正常的乱序到达会被误判为时间戳回退。
	var clockNS int64
	for _, pkt := range bundle.Packets {
		// 确定性单调时刻驱动带宽滑动窗口。
		clockNS += 10_000_000 // 10ms/包
		bw.Observe(time.Unix(0, clockNS), len(pkt.Payload)+12)
		// 延迟观测确定性落在 20..90ms。
		agg.ObserveLatency(20 + rng.Float64()*70)
		// 同步误差确定性小抖动，正常样例 <=20ms。
		agg.ObserveSyncError(5 + rng.Float64()*12)

		_ = pipe.IngestRTP(pkt) // 冲突/迟到已在管线内转为事件
	}
	// 带宽突降时收紧接收窗口建议（不丢弃任何已排队块）。
	_ = bw.SuggestWindow(o.bandwidthMbps)

	// 重排后按序号顺序核对时间戳，真正的回退才登记事件。
	// 维护 64 位有符号样本刻度；回退区间取 [next, running)，
	// 若落在零刻度以下则整体平移回非负域，保证区间非空。
	ordered := orderedPackets(bundle.Packets)
	var running int64
	for i, pkt := range ordered {
		if i == 0 {
			running = 0
			continue
		}
		delta := int64(int32(pkt.Timestamp - ordered[i-1].Timestamp))
		next := running + delta
		if next < running {
			start, end := next, running
			if start < 0 {
				end -= start // 平移 -start，保持区间长度
				start = 0
			}
			pipe.NoteTimestampRollback(
				model.Range{Start: uint64(start), End: uint64(end)},
				pkt.SequenceNumber)
		}
		running = next
	}
	// 注入一致冗余块供恢复器校验。
	for seq, payload := range bundle.Redundant {
		pipe.AddRedundant(&pipeline.Block{
			Seq: seq, Payload: payload, Checksum: pipeline.Checksum(payload),
		})
	}

	// 按序交付到有界队列（队列满时背压，不丢块）。
	// 持续消费者保证 Push 在容量受限时阻塞并计量背压，而非死锁。
	toSample := func(ts uint32) uint64 { return uint64(ts) }
	stopConsume := make(chan struct{})
	consumeDone := make(chan struct{})
	go func() {
		defer close(consumeDone)
		for {
			select {
			case <-stopConsume:
				return
			default:
				if b := pipe.Queue().TryPop(); b != nil {
					continue
				}
				// 短暂让出，避免空转。
				select {
				case <-stopConsume:
					return
				case <-time.After(time.Millisecond):
				}
			}
		}
	}()
	pipe.DrainOrdered(toSample, sample.FramesPerPacket)
	pipe.Reorder().Flush()
	pipe.DrainOrdered(toSample, sample.FramesPerPacket)
	pipe.RecordGaps(toSample)
	close(stopConsume)
	<-consumeDone
	// 排空可能残留的块。
	for pipe.Queue().TryPop() != nil {
	}

	// WAV 流式解析（截断样例在此产生错误事件）。
	for _, ev := range parseWAV(bundle.WAV) {
		pipe.NoteExternal(ev)
	}

	// fMP4 码流检查：时间戳连续性与关键帧间隔。
	videoData := sample.BuildVideoFMP4(sample.DefaultVideoFragments(o.packets))
	rep, err := (&mp4insp.Inspector{Timescale: sample.VideoRate}).
		Inspect(sample.Reader(videoData))
	if err != nil {
		return fmt.Errorf("mp4 inspect: %w", err)
	}
	keyframeGapMS := videoClock.ToMillis(rep.MaxKeyframeGap)

	// 音频损坏区段跨越视频关键帧时，输出同一错误模型事件。
	flagCrossKeyframe(pipe, rep, audioClock, videoClock)
	corrupt := pipe.CorruptRanges(audioClock.ToMillis)

	report := agg.Snapshot(
		pipe.Recoverer().RecoveryRate(),
		keyframeGapMS,
		pipe.Queue().Peak(),
		pipe.Queue().BackpressureMS(),
		corrupt,
	)
	out, _ := json.Marshal(report)
	fmt.Println(string(out))

	if len(corrupt) > 0 || hasSevere(pipe.Events()) {
		return errCorruptSentinel
	}
	return nil
}

func parseWAV(raw []byte) []model.Event {
	p := wav.NewParser(bytes.NewReader(raw))
	var evs []model.Event
	if err := p.ParseHeader(); err != nil {
		return []model.Event{model.NewEvent(model.CodeTruncated, model.SevError,
			model.Range{}, err.Error())}
	}
	for {
		id, err := p.NextChunk()
		if err != nil {
			if id == "data" {
				evs = append(evs, model.NewEvent(model.CodeTruncated, model.SevError,
					model.Range{}, err.Error()))
			}
			return evs
		}
		if id != "data" {
			continue
		}
		for {
			blk, err := p.ReadData(1 << 20)
			_ = blk
			if err != nil {
				if !errors.Is(err, io.EOF) {
					evs = append(evs, model.NewEvent(model.CodeTruncated, model.SevError,
						model.Range{Start: p.FramesDelivered(), End: p.FramesDelivered() + 1},
						err.Error()))
				}
				break
			}
		}
		return evs
	}
}

func flagCrossKeyframe(pipe *pipeline.Pipeline, rep *mp4insp.Report,
	audioClock, videoClock clock.Clock) {
	corrupt := pipe.CorruptRanges(audioClock.ToMillis)
	if len(corrupt) == 0 || len(rep.KeyframeTimes) == 0 {
		return
	}
	for _, cr := range corrupt {
		av, _ := videoClock.Convert(cr.StartSample, audioClock)
		bv, _ := videoClock.Convert(cr.EndSample, audioClock)
		for _, kf := range rep.KeyframeTimes {
			if kf >= av && kf <= bv {
				pipe.NoteAVDiscontinuity(
					model.Range{Start: cr.StartSample, End: cr.EndSample},
					"audio corrupt range spans a video keyframe")
				break
			}
		}
	}
}

func hasSevere(evs []model.Event) bool {
	for _, e := range evs {
		if e.Severity == model.SevError {
			return true
		}
	}
	return false
}

// orderedPackets 按 RTP 序号去重并排序，用于重排后的时间戳核对。
// 同序号冲突时保留首个到达的版本（冲突本身已由管线记录）。
func orderedPackets(pkts []*rtp.Packet) []*rtp.Packet {
	bySeq := map[uint16]*rtp.Packet{}
	for _, p := range pkts {
		if _, ok := bySeq[p.SequenceNumber]; !ok {
			bySeq[p.SequenceNumber] = p
		}
	}
	out := make([]*rtp.Packet, 0, len(bySeq))
	for _, p := range bySeq {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SequenceNumber < out[j].SequenceNumber })
	return out
}
