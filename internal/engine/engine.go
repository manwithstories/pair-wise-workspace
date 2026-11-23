package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"media-pipeline/internal/clock"
	"media-pipeline/internal/demo"
	"media-pipeline/internal/fault"
	"media-pipeline/internal/fingerprint"
	"media-pipeline/internal/ingest"
	"media-pipeline/internal/media"
	"media-pipeline/internal/mux"
	"media-pipeline/internal/resume"
	"media-pipeline/internal/segment"
	"media-pipeline/internal/silence"
	"media-pipeline/internal/wav"
)

// Stats 汇总一个场景的可复核处理结果。
type Stats struct {
	Scenario    string
	Segments    int
	SilenceMS   uint64
	Duplicates  int
	Gaps        int
	BadByte     int64 // -1 表示无损坏
	BadSample   int64 // -1 表示无损坏或损坏位于无法换算处
	ResumeMS    int64
	SegmentSums []string
	GapRanges   []media.Gap
}

// Config 控制引擎处理参数。
type Config struct {
	Queue          int
	TargetFrames   uint64
	WindowFrames   uint64
	MinSilenceFr   uint64
	MaxBlockBytes  int
	InterruptAfter int // 中断前提交的分段数；<=0 表示不中断
}

// DefaultConfig 返回演示使用的确定性默认参数。
func DefaultConfig() Config {
	return Config{
		Queue:          4,
		TargetFrames:   32,
		WindowFrames:   4,
		MinSilenceFr:   2,
		MaxBlockBytes:  1 << 20, // 单块最大 1 MiB
		InterruptAfter: 0,
	}
}

// reorder 把一个场景的 RTP 数据报喂入重排器，返回有序 PCM、缺口与去重计数。
func reorder(scen demo.Scenario, tb clock.Timebase) (pcm []byte, gaps []media.Gap, dupes int, late int, err error) {
	r := ingest.NewReorderer(ingest.ReorderConfig{
		WindowSize: 32, MaxSeqGap: 64,
		FramesPer: demo.FramesPerPacket, BlockID: scen.BlockID,
	})
	fs := tb.FrameSize()
	pcm = make([]byte, 0, len(scen.Datagrams)*demo.FramesPerPacket*fs)
	for _, dg := range scen.Datagrams {
		pkt, perr := ingest.ParsePacket(dg.Bytes, tb)
		if perr != nil {
			return nil, nil, 0, 0, perr
		}
		evs, perr := r.Push(pkt)
		if perr != nil {
			return nil, nil, 0, 0, perr
		}
		for _, ev := range evs {
			switch ev.Kind {
			case ingest.EventPacket:
				pcm = append(pcm, ev.Packet.Payload...)
			case ingest.EventGap:
				gaps = append(gaps, ev.Gap)
			case ingest.EventLate:
				late++
			}
		}
	}
	for _, ev := range r.Flush() {
		switch ev.Kind {
		case ingest.EventPacket:
			pcm = append(pcm, ev.Packet.Payload...)
		case ingest.EventGap:
			gaps = append(gaps, ev.Gap)
		case ingest.EventLate:
			late++
		}
	}
	// 重复包：同一序号出现第二次及以后即计数（重排器已去重，不参与 PCM）。
	dupes = dupCount(scen)
	return pcm, gaps, dupes, late, nil
}

// dupCount 统计数据报中重复序号的出现次数（第二次及以后计数）。
func dupCount(scen demo.Scenario) int {
	seen := map[uint16]int{}
	dup := 0
	for _, dg := range scen.Datagrams {
		seen[dg.Seq]++
		if seen[dg.Seq] > 1 {
			dup++
		}
	}
	return dup
}

// Process 运行单个场景的完整分析与封装，输出 Stats 与机器可读分段清单。
func Process(scen demo.Scenario, cfg Config) (*Stats, []resume.SegmentRef, error) {
	tb := demo.Format()
	stats := &Stats{Scenario: scen.Name, BadByte: -1, BadSample: -1}

	pcm, gaps, dupes, _, err := reorder(scen, tb)
	if err != nil {
		return nil, nil, err
	}
	stats.Gaps = len(gaps)
	stats.GapRanges = gaps
	stats.Duplicates = dupes

	// 非整帧尾块：附加无法成帧的字节，验证精确偏移报告。
	if scen.Trailing > 0 {
		pcm = append(pcm, make([]byte, scen.Trailing)...)
	}

	// WAV 头部与块对齐校验（损坏注入在 data 区）。
	wavBytes := BuildWAV(tb, pcm)
	var injected int = -1
	if scen.InjectOffset > 0 {
		injected = demo.CorruptByte(wavBytes, scen.InjectOffset)
	}
	header, herr := wav.DecodeHeader(scen.BlockID, bytesReader(wavBytes))
	if herr != nil {
		return recordFault(stats, herr, injected)
	}
	dataBytes := wavBytes[header.DataStart : header.DataStart+int64(header.DataSize)]
	if _, cerr := header.CheckPCM(scen.BlockID, dataBytes); cerr != nil {
		return recordFault(stats, cerr, injected)
	}

	// 块大小硬上限。
	if len(dataBytes) > cfg.MaxBlockBytes {
		return nil, nil, fault.New(fault.KindOther, scen.BlockID, -1, -1, false,
			fmt.Sprintf("block %d exceeds max %d", len(dataBytes), cfg.MaxBlockBytes)).
			WithOp("engine")
	}

	block := media.Block{
		ID: scen.BlockID, Format: tb, FirstSample: 0,
		PCM: dataBytes, Gaps: gaps, SourceSha: demo.HashBytes(dataBytes),
	}

	// 静音检测（跨块一致），用区间中点向切分器注册静音切点。
	det, err := silence.New(silence.Config{
		WindowFrames:     cfg.WindowFrames,
		MinSilenceFrames: cfg.MinSilenceFr,
		PeakThreshold:    50_000,
		RMSThreshold:     50_000,
		Channels:         tb.Channels,
		BitsPerSample:    tb.BitsPerSample,
		Rule:             silence.RuleMax,
	}, tb)
	if err != nil {
		return nil, nil, err
	}
	ivs, err := det.Push(block)
	if err != nil {
		return nil, nil, err
	}
	ivs = append(ivs, det.Flush()...)

	splitter := segment.New(segment.Config{
		TargetFrames: cfg.TargetFrames, MinFrames: 1,
	}, tb.FrameSize())
	for _, iv := range ivs {
		stats.SilenceMS += tb.FramesToMillis(iv.Frames)
		mid := iv.FirstSample + iv.Frames/2
		splitter.AddSilenceBoundary(segment.SilenceCut{Sample: mid})
	}

	// 指纹：非重叠窗口，登记内容级重复（与 RTP 包重复区分，单独累计）。
	hasher := fingerprint.New(tb, cfg.WindowFrames)
	if _, err := hasher.Push(block); err != nil {
		return nil, nil, err
	}
	contentDupes := len(hasher.Duplicates())

	segs, err := splitter.Push(block)
	if err != nil {
		return nil, nil, err
	}
	segs = append(segs, splitter.Flush()...)

	muxer, err := mux.New(tb, 1)
	if err != nil {
		return nil, nil, err
	}
	refs := make([]resume.SegmentRef, 0, len(segs))
	for i, sg := range segs {
		sum := sha256.Sum256(sg.PCM)
		// 封装并回读核验，确保 mdat 与源 PCM 逐字节一致。
		var sink memReadWriteSeeker
		if err := muxer.WriteSegment(&sink, uint32(i+1), sg.FirstSample, sg.PCM); err != nil {
			return nil, nil, err
		}
		if _, err := mux.VerifyReadback(newReader(sink.bytes()), sg.PCM); err != nil {
			return nil, nil, err
		}
		refs = append(refs, resume.SegmentRef{
			Index: sg.Index, FirstSample: sg.FirstSample,
			EndSample: sg.EndSample, Sha256: hex.EncodeToString(sum[:]),
		})
		stats.SegmentSums = append(stats.SegmentSums, hex.EncodeToString(sum[:]))
	}
	stats.Segments = len(segs)
	// RTP 传输重复（dupes）与内容指纹重复（contentDupes）分别统计后汇总。
	stats.Duplicates = dupes + contentDupes
	return stats, refs, nil
}

// recordFault 把结构化损坏错误转写为 Stats 的字节/采样定位。
func recordFault(stats *Stats, err error, injected int) (*Stats, []resume.SegmentRef, error) {
	fe, ok := fault.As(err)
	if !ok {
		return nil, nil, err
	}
	stats.BadByte = fe.ByteOffset
	stats.BadSample = fe.SampleOffset
	if injected >= 0 && stats.BadByte >= 0 {
		// 验收允许偏移与注入点相差不超过 1 字节。
		if d := stats.BadByte - int64(injected); d < -1 || d > 1 {
			return nil, nil, err
		}
	}
	return stats, nil, nil
}
