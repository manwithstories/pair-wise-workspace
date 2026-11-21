package pipeline

import (
	"encoding/hex"
	"sort"

	"archivecheck/internal/clock"
	"archivecheck/internal/ingest"
	"archivecheck/internal/ladder"
	"archivecheck/internal/model"
	"archivecheck/internal/segment"
	"archivecheck/internal/transport"
)

// sortedReps 返回字典序表示层名称，消除 map 遍历的不确定性。
func sortedReps(frames map[string][]model.Frame) []string {
	reps := make([]string, 0, len(frames))
	for rep := range frames {
		reps = append(reps, rep)
	}
	sort.Strings(reps)
	return reps
}

// runTransport 对每个表示层执行故障注入与有界窗口恢复，汇总传输统计。
func runTransport(c Config, frames map[string][]model.Frame, rep *Report) error {
	var injected, recovered, unrecovered int
	var allGaps []model.Gap
	for _, name := range sortedReps(frames) {
		fs := frames[name]
		seqs := make([]uint32, len(fs))
		for i, f := range fs {
			seqs[i] = f.Seq
		}
		pkts := transport.Inject(seqs, func(uint32) bool { return false }, c.Fault)
		w := transport.NewWindow(c.WindowSize)
		for _, p := range pkts {
			w.Push(p)
		}
		st, gaps := w.Flush()
		injected += st.Injected
		recovered += st.Recovered
		unrecovered += st.Unrecovered
		for _, g := range gaps {
			allGaps = append(allGaps, model.Gap{Start: g.Start, End: g.End})
		}
	}
	if injected > 0 {
		rep.RecoveryRate = float64(recovered) / float64(injected)
	} else {
		rep.RecoveryRate = 1
	}
	rep.Gaps = allGaps
	_ = unrecovered
	return nil
}

// layerContext 承载单层恢复的共享中间产物。
type layerContext struct {
	prefix   []model.Frame
	timeline *clock.Timeline
	points   []segment.MediaPoint
}

// runVerifyAndTimeline 执行输入校验、传输恢复打标与共享时间线构建。
// 表示层按字典序处理，保证多运行结果确定；每帧只扩展一次时间戳。
func runVerifyAndTimeline(c Config, frames map[string][]model.Frame, rep *Report) (map[string]*layerContext, error) {
	latencies := make([]model.LatencySample, 0, c.WindowSize*2)
	out := map[string]*layerContext{}
	for _, name := range sortedReps(frames) {
		fs := frames[name]
		rd := ingest.NewReader(name, c.Params.WrapAt)
		rd.OnVerified = func(seq uint32, latNS int64) {
			latencies = appendBounded(latencies, model.LatencySample{Seq: seq, LatNS: latNS}, c.WindowSize*4)
		}
		prefix, firstErr := rd.Verify(fs)
		if firstErr != nil {
			rep.FirstError = firstErr
			rep.P95LatencyNS = model.P95Latency(latencies)
			return nil, nil // 保留可信前缀语义：首错即停，不发布
		}
		checks := ingest.Repair(prefix, rep.Gaps)
		_ = checks

		tl := clock.NewTimeline(clock.TimelineConfig{
			AudioRate:            c.Params.AudioRate,
			IsolationThresholdNS: 20_000_000,
		})
		uw := clock.NewUnwrapper(c.Params.WrapAt)
		type extPoint struct {
			f   model.Frame
			ext int64
		}
		extPoints := make([]extPoint, 0, len(prefix))
		for _, f := range prefix {
			ext, _ := uw.Extend(f.RTPTimestamp)
			tl.AddPoint(f.Seq, int64(ext), f.AudioSamples)
			extPoints = append(extPoints, extPoint{f: f, ext: int64(ext)})
		}
		if err := tl.Build(); err != nil {
			return nil, err
		}
		points := make([]segment.MediaPoint, 0, len(extPoints))
		for _, ep := range extPoints {
			ns, _ := tl.MediaNS(ep.f.Seq, ep.ext)
			points = append(points, segment.MediaPoint{Seq: ep.f.Seq, IsKey: ep.f.IsKey, MediaNS: ns})
		}
		if r := tl.MaxResidual(); r > rep.SyncErrorNS {
			rep.SyncErrorNS = r
		}
		out[name] = &layerContext{prefix: prefix, timeline: tl, points: points}
	}
	rep.P95LatencyNS = model.P95Latency(latencies)
	return out, nil
}

// runSegmentAndMerge 生成分片清单并做跨表示层边界核验，决定是否发布。
func runSegmentAndMerge(c Config, frames map[string][]model.Frame, ctx map[string]*layerContext, rep *Report) error {
	bcfg := segment.BoundaryConfig{
		TargetNS:    c.SegmentTargetNS,
		ToleranceNS: c.BoundaryTolNS,
		MinKeyGap:   1,
	}
	var ladderResults []ladder.RepResult
	for _, name := range sortedReps(frames) {
		lc := ctx[name]
		bounds := segment.SelectBoundaries(lc.points, bcfg)
		m := segment.Build(name, lc.prefix, lc.points, bounds, bcfg)
		m.Gaps = rep.Gaps
		rep.ManifestByRep[name] = m
		rep.Segments += len(m.Segments)
		ladderResults = append(ladderResults, ladder.RepResult{Rep: name, Prefix: lc.prefix, Manifest: m})
	}
	isolated := func(name string, seq uint32) bool {
		return ctx[name].timeline.IsolatedAt(seq)
	}
	report := ladder.Fanin(ladderResults, c.BoundaryTolNS, isolated)
	rep.BoundaryMiss = len(report.Mismatches)
	if !report.Accepted {
		rep.RejectedReason = "boundary"
		for _, r := range ladderResults {
			if last := r.Manifest.Segments[len(r.Manifest.Segments)-1]; last.TailIncomplete {
				rep.RejectedReason = "tail_truncated"
				break
			}
		}
		return nil
	}
	// 以字典序首层清单作为发布清单，其哈希即验收口径的 manifest_sha256。
	first := rep.ManifestByRep[report.Reps[0]]
	digest := segment.Hash(first)
	rep.ManifestHash = hex.EncodeToString(digest[:])
	rep.Published = true
	rep.OK = true
	return nil
}

// appendBounded 维持有界采样窗口，超出容量丢弃最旧样本。
func appendBounded(s []model.LatencySample, v model.LatencySample, cap int) []model.LatencySample {
	s = append(s, v)
	if len(s) > cap {
		s = s[len(s)-cap:]
	}
	return s
}
