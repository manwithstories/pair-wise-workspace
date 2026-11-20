// Package pipeline 编排解析、修复、索引、验证与报告，贯穿有界队列背压。
// 任何阶段错误都会以 fault.Error 上报原始字节偏移、轨道、阶段与可恢复性。
package pipeline

import (
	"context"
	"io"
	"path/filepath"
	"sort"
	"time"

	"mediarepair/internal/fault"
	"mediarepair/internal/index"
	"mediarepair/internal/model"
	"mediarepair/internal/parse"
	"mediarepair/internal/repair"
	"mediarepair/internal/report"
	"mediarepair/internal/stream"
)

// Options 是流水线运行参数。
type Options struct {
	Input        string          // MP4 路径
	TimelinePath string          // 时间线 sidecar 路径（空则取同目录 timeline.json）
	Buffer       int             // 有界队列容量
	SlowConsumer int             // 消费者每样本延迟毫秒，用于演示背压
	Strategy     repair.Strategy // 修复策略
	ResumeFrom   string          // 续跑边界：该片段及之前的片段跳过
	SampleSink   io.Writer       // 可选：修复后样本的稳定编码输出
	Context      context.Context // 取消信号
}

// Outcome 是流水线产物。
type Outcome struct {
	Result   *report.Result
	Timeline *model.Timeline
	Segments []repair.SegmentInfo
	Verified []string // 本次实际处理的片段 ID（去重、按出现顺序）
	Stats    stream.Stats
	RTP      RTPReport
}

// Run 执行完整流水线。返回错误可能是 *fault.Error。
func Run(opts Options) (*Outcome, error) {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Buffer < 1 {
		return nil, fault.New(fault.StageIO, 0, -1, false, "缓冲容量必须 >= 1")
	}
	mf, err := parse.OpenMP4(opts.Input)
	if err != nil {
		return nil, err
	}
	defer mf.Close()

	tlPath := opts.TimelinePath
	if tlPath == "" {
		tlPath = filepath.Join(filepath.Dir(opts.Input), "timeline.json")
	}
	tl, err := parse.LoadTimeline(tlPath)
	if err != nil {
		return nil, err
	}

	clock := repair.NewClock(tl, opts.Strategy)
	ix := index.New()
	hasher := report.NewHasher()
	q := stream.NewQueue[model.Sample](opts.Buffer)
	out := &Outcome{Timeline: tl}

	skip := resumeSet(tl, opts.ResumeFrom)
	verified := make(map[string]bool)
	var restored []model.RepairedSample
	var roundtripHit, roundtripTotal uint64

	// 生产者：流式解析并推入有界队列；队列满即阻塞形成背压。
	prodErr := make(chan error, 1)
	go func() {
		err := mf.StreamSamples(ctx, func(s model.Sample) error {
			return q.Push(ctx, s)
		})
		q.Close()
		prodErr <- err
	}()

	// 消费者：弹出、修复、索引、哈希。
	var runErr error
	for {
		s, ok := q.Pop(ctx)
		if !ok {
			break
		}
		if skip[segmentOfSample(tl, s)] {
			continue
		}
		trk, _ := tl.TrackByID(s.TrackID)
		rs, err := clock.Process(s)
		if err != nil {
			runErr = err
			break
		}
		if err := ix.AddSample(rs); err != nil {
			runErr = err
			break
		}
		if err := hasher.Add(rs); err != nil {
			runErr = err
			break
		}
		if opts.SampleSink != nil {
			if err := report.WriteSamples(opts.SampleSink, rs); err != nil {
				runErr = fault.Wrap(fault.StageReport, rs.TrackID, rs.SourceOffset, false, "输出样本失败", err)
				break
			}
		}
		roundtripTotal++
		if back, err := model.FromUnified(trk.Timescale, rs.UnifiedDTS); err == nil && back == rs.RepairedDTS {
			roundtripHit++
		}
		meta := rs
		meta.Data = nil
		restored = append(restored, meta)
		if rs.SegmentID != "" {
			verified[rs.SegmentID] = true
		}
		if opts.SlowConsumer > 0 {
			if err := sleepCtx(ctx, opts.SlowConsumer); err != nil {
				runErr = fault.Wrap(fault.StageIO, rs.TrackID, -1, false, "流水线已取消", err)
				break
			}
		}
	}
	// 终止生产者：关闭队列会唤醒其阻塞的 Push，避免泄漏。
	q.Close()
	prodErrVal := <-prodErr
	if runErr == nil && prodErrVal != nil {
		runErr = prodErrVal
	}
	if runErr == nil && ctx.Err() != nil {
		runErr = fault.Wrap(fault.StageIO, 0, -1, false, "流水线已取消", ctx.Err())
	}

	rtpRep, rtpErr := processRTP(ctx, opts.Input, tl, opts.Strategy)
	if rtpErr != nil {
		return out, rtpErr
	}
	out.RTP = rtpRep

	// 已验证片段仍可输出：先汇总已处理部分，再按需返回错误。
	ix.Finalize()
	out.Segments = clock.Segments()
	for _, s := range out.Segments {
		if verified[s.ID] {
			out.Verified = append(out.Verified, s.ID)
		}
	}
	out.Stats = q.Stats()
	res := buildResult(opts, tl, clock, ix, hasher.Finalize(), out, restored, roundtripHit, roundtripTotal)
	res.RTP = &out.RTP
	if runErr != nil {
		markFailure(res, runErr)
	}
	res.Summary = res.SummaryText()
	out.Result = res
	if runErr != nil {
		return out, runErr
	}
	return out, nil
}

// markFailure 把错误定位写入报告。
func markFailure(res *report.Result, err error) {
	if fe, ok := fault.As(err); ok {
		res.MarkFailure(string(fe.Stage), fe.Offset, fe.Recoverable, fe.Error())
		return
	}
	res.MarkFailure(string(fault.StageIO), -1, false, err.Error())
}

// resumeSet 返回续跑时应跳过的片段集合：验证边界及其之前的所有片段。
func resumeSet(tl *model.Timeline, boundary string) map[string]bool {
	if boundary == "" {
		return nil
	}
	skip := make(map[string]bool)
	for _, c := range tl.Clips {
		skip[c.ID] = true
		if c.ID == boundary {
			return skip
		}
	}
	for _, tr := range tl.Transitions {
		skip[tr.ID] = true
		if tr.ID == boundary {
			return skip
		}
	}
	return nil // 边界不存在：不跳过任何片段，避免误吞数据
}

// segmentOfSample 按样本区间查找所属片段。
func segmentOfSample(tl *model.Timeline, s model.Sample) string {
	for _, c := range tl.Clips {
		if c.TrackID == s.TrackID && s.Seq >= c.FirstSample && s.Seq < c.FirstSample+c.SampleCount {
			return c.ID
		}
	}
	for _, tr := range tl.Transitions {
		if tr.TrackID == s.TrackID && s.Seq >= tr.FirstSample && s.Seq < tr.FirstSample+tr.SampleCount {
			return tr.ID
		}
	}
	return ""
}

// buildResult 汇总修复、索引命中、缓冲与哈希统计。
func buildResult(opts Options, tl *model.Timeline, clock *repair.Clock, ix *index.Index,
	hashes []report.SegmentHash, out *Outcome, restored []model.RepairedSample,
	rtHit, rtTotal uint64) *report.Result {

	stats := clock.Stats()
	byTrack := make(map[uint32]repair.TrackStats, len(stats))
	for _, s := range stats {
		byTrack[s.TrackID] = s
	}
	keyCount := make(map[uint32]int)
	for _, t := range tl.Tracks {
		keyCount[t.ID] = len(ix.Entries(t.ID))
	}
	res := &report.Result{
		Schema:            "mediarepair/report/v1",
		Input:             opts.Input,
		BufferCapacity:    opts.Buffer,
		PeakBufferSamples: out.Stats.PeakLen,
		BackpressureWait:  out.Stats.BackpressureW.Milliseconds(),
		SegmentHashes:     hashes,
		SegmentHashCount:  len(hashes),
	}
	if rtTotal > 0 {
		res.RoundtripMatch = float64(rtHit) / float64(rtTotal)
	}
	res.KeyframeHitRate = measureHitRate(ix, tl, restored)
	seeks, hits := ix.Stats()

	for _, t := range tl.Tracks {
		ts := byTrack[t.ID]
		res.Tracks = append(res.Tracks, report.TrackReport{
			TrackID: t.ID, Timescale: t.Timescale, Kind: string(t.Kind),
			Samples: ts.Samples, Wraps: ts.Wraps, Duplicates: ts.Duplicates,
			Regressions: ts.Regressions, ClipJumps: ts.ClipJumps, Drifts: ts.Drifts,
			Keyframes: keyCount[t.ID], Seeks: seeks, Hits: hits, HitRate: ix.HitRate(),
		})
	}
	sort.Slice(res.Tracks, func(i, j int) bool { return res.Tracks[i].TrackID < res.Tracks[j].TrackID })
	for _, s := range out.Segments {
		res.Segments = append(res.Segments, report.SegmentReport{
			ID: s.ID, TrackID: s.TrackID, Transition: s.IsTransition,
			StartTick: s.StartTick, EndTick: s.EndTick, Samples: s.Samples, Repairs: s.Repairs,
		})
	}
	return res
}

// measureHitRate 对每个样本查询最近关键帧，命中条件为：存在条目且该条目
// 归属同一片段（转场/剪辑内可随机访问）。无样本时返回 1。
func measureHitRate(ix *index.Index, tl *model.Timeline, samples []model.RepairedSample) float64 {
	if len(samples) == 0 {
		return 1
	}
	var hit, total uint64
	for _, s := range samples {
		total++
		e, ok := ix.Seek(s.TrackID, s.UnifiedDTS)
		if ok && e.SegmentID == s.SegmentID {
			hit++
		}
	}
	return float64(hit) / float64(total)
}

func sleepCtx(ctx context.Context, ms int) error {
	if ms <= 0 {
		return nil
	}
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
