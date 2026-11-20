package app

import (
	"fmt"
	"strings"

	"mediagraph/internal/pipeline"
	"mediagraph/internal/process"
	"mediagraph/internal/provenance"
	mgrtp "mediagraph/internal/rtp"
	"mediagraph/internal/subtitle"
	"mediagraph/internal/testkit"
	"mediagraph/internal/timeline"
)

// builder 为步骤闭包共享的运行环境。
type builder struct {
	fx  *testkit.Fixtures
	cfg *pipeline.Config
}

// steps 构造显式轨道依赖图：字幕/传输/时间码/音轨为独立轨道，
// 切分依赖 MP4 时基与时间码，指纹依赖切分，静音依赖音量序列。
func (b *builder) steps() []pipeline.Step {
	return []pipeline.Step{
		{ID: stepSRTGood, Track: "subtitle", Run: b.parseFile(stepSRTGood, testkit.GoodSRT)},
		{ID: stepSRTCorrupt, Track: "subtitle", Run: b.parseFile(stepSRTCorrupt, testkit.CorruptSRT)},
		{ID: stepVTT, Track: "subtitle", Run: b.parseFile(stepVTT, testkit.GoodVTT)},
		{ID: stepRTPGood, Track: "transport", Run: b.rtpFile(stepRTPGood, testkit.RTPGood)},
		{ID: stepRTPCorrupt, Track: "transport", Run: b.rtpFile(stepRTPCorrupt, testkit.RTPCorrupt)},
		{ID: stepMP4Good, Track: "transport", Run: b.mp4File(stepMP4Good, testkit.MP4Good)},
		{ID: stepMP4Corrupt, Track: "transport", Run: b.mp4File(stepMP4Corrupt, testkit.MP4Corrupt)},
		{ID: stepTimecode, Track: "timecode", Run: b.timecodes},
		{ID: stepLevels, Track: "audio", Run: b.levels},
		{ID: stepSplit, Track: "video", Deps: []string{stepMP4Good, stepTimecode}, Run: b.split},
		{ID: stepSilence, Track: "audio", Deps: []string{stepLevels}, Run: b.silence},
		{ID: stepDedup, Track: "video", Deps: []string{stepSplit}, Run: b.dedup},
	}
}

// recordErr 追加一条带字节偏移的溯源错误记录。
func recordErr(ctx *pipeline.Ctx, step, track, file string, offset int64, msg string) {
	ctx.Log.Add(provenance.Record{Step: step, Track: track, SourceFile: file,
		ByteStart: offset, ByteEnd: offset, Errors: []string{msg}}, nil)
}

// parseFile 解析 SRT/WebVTT：可用字幕逐条记录字节与时间范围，
// 可恢复错误（非法时间/重叠/截断）记录源字节偏移后继续。
func (b *builder) parseFile(id, name string) func(*pipeline.Ctx) error {
	return func(ctx *pipeline.Ctx) error {
		data, err := pipeline.ReadInput(b.fx.Dir, name)
		if err != nil {
			return err
		}
		res := subtitle.Parse(data)
		for _, c := range res.Cues {
			ctx.Log.Add(provenance.Record{Step: id, Track: "subtitle", SourceFile: name,
				ByteStart: c.ByteStart, ByteEnd: c.ByteEnd,
				TicksStart: c.Start, TicksEnd: c.End}, []byte(c.Text))
		}
		first := int64(-1)
		for _, pe := range res.Errors {
			if first < 0 || pe.Offset < first {
				first = pe.Offset
			}
			recordErr(ctx, id, "subtitle", name, pe.Offset, pe.Error())
		}
		return ctx.State.MarkDone(id, parseResult{
			Format: res.Format, Cues: len(res.Cues), Errors: len(res.Errors),
			FirstErrorOffset: first,
		})
	}
}

// rtpFile 校验 RTP 记录流的包序与时间戳；单包问题不中断后续校验。
func (b *builder) rtpFile(id, name string) func(*pipeline.Ctx) error {
	return func(ctx *pipeline.Ctx) error {
		data, err := pipeline.ReadInput(b.fx.Dir, name)
		if err != nil {
			return err
		}
		res := mgrtp.Ingest(data)
		for _, is := range res.Issues {
			recordErr(ctx, id, "transport", name, is.Offset, is.Error())
		}
		return ctx.State.MarkDone(id, rtpResult{
			Packets: res.Packets, Gaps: res.SeqGaps, Duplicates: res.Duplicates,
			Reorders: res.Reorders, Wraps: res.TimestampWraps,
			Issues: len(res.Issues), FirstErrorOffset: res.FirstErrorOffset,
		})
	}
}

// mp4File 读取自生成 MP4 的轨道时基并校验样本表一致性。
func (b *builder) mp4File(id, name string) func(*pipeline.Ctx) error {
	return func(ctx *pipeline.Ctx) error {
		data, err := pipeline.ReadInput(b.fx.Dir, name)
		if err != nil {
			return err
		}
		info := mgrtp.ReadMP4(data)
		for _, is := range info.Issues {
			recordErr(ctx, id, "transport", name, is.Offset,
				fmt.Sprintf("%s at byte %d: %s", is.Kind, is.Offset, is.Detail))
		}
		return ctx.State.MarkDone(id, mp4Result{
			Timescale: info.Timescale, Samples: info.SampleCount,
			Issues: len(info.Issues), FirstErrorOffset: info.FirstErrorOffset,
		})
	}
}

// rateTable 为时间码文件支持的帧率制式。
var rateTable = map[string]timeline.Rate{
	"24":         timeline.Rate24,
	"25":         timeline.Rate25,
	"30":         timeline.Rate30,
	"30000/1001": timeline.RateNTSC,
}

// timecodes 解析 "帧率 时间码" 行并换算到统一时间基准；
// 非法行记录字节偏移与原始文本后继续。
func (b *builder) timecodes(ctx *pipeline.Ctx) error {
	data, err := pipeline.ReadInput(b.fx.Dir, testkit.Timecodes)
	if err != nil {
		return err
	}
	res := timecodeResult{FirstErrorOffset: -1}
	fail := func(offset int64, msg string) {
		res.Errors++
		if res.FirstErrorOffset < 0 {
			res.FirstErrorOffset = offset
		}
		recordErr(ctx, stepTimecode, "timecode", testkit.Timecodes, offset, msg)
	}
	off := int64(0)
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		start := off
		off += int64(len(raw)) + 1
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			fail(start, fmt.Sprintf("malformed timecode line %q", line))
			continue
		}
		rate, ok := rateTable[fields[0]]
		if !ok {
			fail(start, fmt.Sprintf("unknown rate %q", fields[0]))
			continue
		}
		ticks, wraps, terr := timeline.ParseTimecode(fields[1], rate)
		if terr != nil {
			fail(start, terr.Error())
			continue
		}
		res.Converted++
		ctx.Log.Add(provenance.Record{Step: stepTimecode, Track: "timecode",
			SourceFile: testkit.Timecodes,
			ByteStart:  start, ByteEnd: start + int64(len(line)),
			TicksStart: ticks, TicksEnd: ticks},
			[]byte(fmt.Sprintf("%s wraps=%d", line, wraps)))
	}
	return ctx.State.MarkDone(stepTimecode, res)
}

// levels 读取逐桶音量序列（供静音检测）。
func (b *builder) levels(ctx *pipeline.Ctx) error {
	data, err := pipeline.ReadInput(b.fx.Dir, testkit.Levels)
	if err != nil {
		return err
	}
	var lv []float64
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var v float64
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "%g", &v); err != nil {
			return fmt.Errorf("levels: bad line %q: %w", line, err)
		}
		lv = append(lv, v)
	}
	if len(lv) > pipeline.MaxCues {
		return fmt.Errorf("levels: %d buckets exceed limit %d", len(lv), pipeline.MaxCues)
	}
	return ctx.State.MarkDone(stepLevels, levelsResult{Levels: lv})
}

// split 按时间窗切分媒体总时长，保留首尾边界与确定的原因标记。
func (b *builder) split(ctx *pipeline.Ctx) error {
	if b.fx.MediaTicks > pipeline.MaxSpanTicks {
		return fmt.Errorf("media span %d ticks exceeds limit %d", b.fx.MediaTicks, pipeline.MaxSpanTicks)
	}
	window := timeline.MillisToTicks(ctx.Cfg.WindowMs)
	minWin := timeline.MillisToTicks(ctx.Cfg.MinWindowMs)
	segs, err := process.Split(0, b.fx.MediaTicks, window, minWin)
	if err != nil {
		return err
	}
	for _, s := range segs {
		ctx.Log.Add(provenance.Record{Step: stepSplit, Track: "video",
			ByteStart: -1, ByteEnd: -1, TicksStart: s.Start, TicksEnd: s.End},
			[]byte(fmt.Sprintf("seg %d [%d,%d) %s", s.Index, s.Start, s.End, s.Reason)))
	}
	return ctx.State.MarkDone(stepSplit, splitResult{Segments: segs})
}

// silence 用滑动窗与滞回阈值检测静音区间，统一到主时间基准。
func (b *builder) silence(ctx *pipeline.Ctx) error {
	var lv levelsResult
	if !ctx.State.Get(stepLevels, &lv) {
		return fmt.Errorf("missing %s result", stepLevels)
	}
	iv, err := process.DetectSilence(lv.Levels, b.fx.BucketTicks, 0.1, 0.3, 3)
	if err != nil {
		return err
	}
	for _, v := range iv {
		ctx.Log.Add(provenance.Record{Step: stepSilence, Track: "audio",
			ByteStart: -1, ByteEnd: -1, TicksStart: v.Start, TicksEnd: v.End},
			[]byte(fmt.Sprintf("silence [%d,%d)", v.Start, v.End)))
	}
	return ctx.State.MarkDone(stepSilence, silenceResult{
		Intervals: len(iv), SilenceMs: timeline.TicksToMillis(process.TotalTicks(iv))})
}

// dedup 按规范化媒体时间窗生成指纹并跨轨去重：
// edge-a/edge-b 承载相同内容（模拟跨节点重复分发），edge-c 内容不同。
func (b *builder) dedup(ctx *pipeline.Ctx) error {
	var sp splitResult
	if !ctx.State.Get(stepSplit, &sp) {
		return fmt.Errorf("missing %s result", stepSplit)
	}
	var entries []process.Entry
	for _, s := range sp.Segments {
		base := fmt.Sprintf("content|%d|%d|%d", ctx.Cfg.Seed, s.Start, s.End)
		for _, tr := range []string{"edge-a", "edge-b"} {
			entries = append(entries, process.Entry{TrackID: tr, Seg: s,
				ContentDigest: process.DigestSamples([]byte(base))})
		}
		entries = append(entries, process.Entry{TrackID: "edge-c", Seg: s,
			ContentDigest: process.DigestSamples([]byte("c|" + base))})
	}
	res := process.Dedup(entries)
	ctx.Log.Add(provenance.Record{Step: stepDedup, Track: "video",
		ByteStart: -1, ByteEnd: -1},
		[]byte(fmt.Sprintf("kept=%d deduplicated=%d collisions=%d",
			len(res.Kept), res.Deduplicated, res.Collisions)))
	return ctx.State.MarkDone(stepDedup, dedupResult{
		Kept: len(res.Kept), Deduplicated: res.Deduplicated, Collisions: res.Collisions})
}
