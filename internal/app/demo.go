package app

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"bcast/internal/fault"
	"bcast/internal/model"
	"bcast/internal/probe"
	"bcast/internal/render"
	"bcast/internal/rendition"
	"bcast/internal/resume"
	"bcast/internal/timeline"
	"bcast/internal/verify"
)

// RunDemo 执行完整演示流水线并返回确定性核验报告。
// 错误以 fault 哨兵分类，调用方据 verify.Is* 映射退出码。
func RunDemo(o Options) (*verify.Report, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.WorkDir, 0o755); err != nil {
		return nil, err
	}
	durationMS := o.Duration.Milliseconds()
	durationUS := durationMS * 1000
	segmentUS := SegmentMS * 1000

	// 1) 生成确定性素材（漂移/回绕/VFR/丢包/重复/损坏/截断）。
	tracks := probe.DefaultTracks()
	plan := model.Plan{
		Seed: o.Seed, DurationMS: durationMS, Tracks: tracks,
		WrapModulus: probe.DefaultWrapModulus(tracks),
		Faults:      probe.DefaultFaultPlan(o.FaultsAll), SegmentMS: SegmentMS,
	}
	masterPath := filepath.Join(o.WorkDir, "master.bin")
	if _, err := probe.Generate(o.WorkDir, plan); err != nil {
		return nil, err
	}
	sourceSHA, err := resume.FileSHA256(masterPath)
	if err != nil {
		return nil, err
	}

	// 2) 探针读取 → 时间线合并（回绕展开、去重、归一时基、缺口）。
	fc := &fault.Collector{}
	src, err := probe.Open(masterPath, fc)
	if err != nil {
		return nil, err
	}
	tl, err := timeline.Build(src, fc)
	if err != nil {
		src.Close()
		return nil, err
	}
	if err := src.Close(); err != nil {
		return nil, err
	}
	drifts := timeline.EstimateAll(tl)

	// 3) 三档表示层扇出 + 共享时钟扇入核验。
	tiers := rendition.DefaultTiers()
	rends, err := rendition.FanOut(tl, tiers, fc)
	if err != nil {
		return nil, err
	}
	fanIn, err := rendition.FanIn(rends, fc)
	if err != nil {
		return nil, err
	}

	// 4) 离屏帧序列导出（PNG + BMP 旁证）与像素哈希。
	specs := render.PlanFrames(durationUS, o.ReviewStep.Microseconds(), MaxFrames)
	video := tl.Track(0)
	refs, err := render.Export(o.WorkDir, specs, sampleAt(video))
	if err != nil {
		return nil, err
	}
	pixelSHA := verify.AggregatePixelHash(refs)
	badFrames, err := verify.VerifyFrames(refs)
	if err != nil {
		return nil, err
	}

	// 5) 确定性封装：完整运行与中断续跑两条路径。
	manifest := resume.Manifest{
		SegmentUS: segmentUS, SampleCount: len(tl.Samples),
		GapCount: len(tl.Gaps), FaultCount: fc.Count(), SourceSHA256: sourceSHA,
	}
	fullPath := filepath.Join(o.WorkDir, "full.mpk")
	resumedPath := filepath.Join(o.WorkDir, "resumed.mpk")
	fullJPath := fullPath + ".journal.json"
	resJPath := resumedPath + ".journal.json"

	fullWriter, err := resume.NewFullWriter(fullPath, fullJPath, sourceSHA, segmentUS, nil)
	if err != nil {
		return nil, err
	}
	if err := fullWriter.WriteAll(tl.Samples, tl.Gaps, fc.Items(), manifest); err != nil {
		fullWriter.Close()
		return nil, err
	}
	if err := fullWriter.Close(); err != nil {
		return nil, err
	}

	cutUS := durationUS * int64(o.ResumeCutPct) / 100
	resumeMS, err := runResumed(resumedPath, resJPath, sourceSHA, segmentUS,
		tl, manifest, cutUS)
	if err != nil {
		return nil, err
	}

	// 6) 独立核验：块哈希链、整文件校验和、边界。
	fullJ, err := resume.Load(fullJPath)
	if err != nil {
		return nil, err
	}
	resJ, err := resume.Load(resJPath)
	if err != nil {
		return nil, err
	}
	blockOK := true
	if _, err := verify.VerifyPackage(fullPath, fullJ); err != nil {
		blockOK = false
	}
	if _, err := verify.VerifyPackage(resumedPath, resJ); err != nil {
		blockOK = false
	}
	fullSHA, resumedSHA, identical, err := verify.ComparePackages(fullPath, resumedPath)
	if err != nil {
		return nil, err
	}

	rep := verify.Report{
		Tracks:                 len(plan.Tracks),
		Renditions:             len(tiers),
		Frames:                 len(refs),
		FaultLocationErrorPkts: countUnlocatable(fc.Items()),
		BoundaryMismatches:     fanIn.BoundaryMismatches,
		CrossLayerDeviations:   fanIn.CrossLayerDeviations,
		ResumeMS:               resumeMS,
		PixelSHA256:            pixelSHA,
		FullSHA256:             fullSHA,
		ResumedSHA256:          resumedSHA,
		PackagesIdentical:      identical,
		BlockVerificationOK:    blockOK,
		FrameVerificationOK:    badFrames == 0,
		DriftPPM:               ppmList(drifts),
	}
	if err := writeManifests(o.WorkDir, tl, refs, fc); err != nil {
		return nil, err
	}
	if err := verify.WriteReport(filepath.Join(o.WorkDir, "verify_report.json"), rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// runResumed 先在 cutUS 中断，再续跑至完成，返回续跑实际耗时（毫秒）。
func runResumed(path, jPath, sourceSHA string, segmentUS int64,
	tl *timeline.Timeline, m resume.Manifest, cutUS int64) (int64, error) {
	w, err := resume.NewFullWriter(path, jPath, sourceSHA, segmentUS, nil)
	if err != nil {
		return 0, err
	}
	w.SetCutPoint(cutUS)
	err = w.WriteAll(tl.Samples, tl.Gaps, nil, m)
	if !errors.Is(err, resume.ErrCut) {
		w.Close()
		return 0, err
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	j, err := resume.Load(jPath)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	rw, err := resume.NewFullWriter(path, jPath, sourceSHA, segmentUS, j)
	if err != nil {
		return 0, err
	}
	if err := rw.WriteAll(tl.Samples, tl.Gaps, nil, m); err != nil {
		rw.Close()
		return 0, err
	}
	if err := rw.Close(); err != nil {
		return 0, err
	}
	return time.Since(start).Milliseconds(), nil
}

// sampleAt 返回在帧时刻选取最近视频样本（轨道 0）的确定性函数。
func sampleAt(video []model.Sample) func(render.FrameSpec) model.Sample {
	return func(spec render.FrameSpec) model.Sample {
		idx := sort.Search(len(video), func(i int) bool { return video[i].PTS >= spec.TimeUS })
		if idx >= len(video) {
			idx = len(video) - 1
		}
		if idx > 0 {
			if d1 := spec.TimeUS - video[idx-1].PTS; video[idx].PTS-spec.TimeUS > d1 {
				idx--
			}
		}
		if len(video) == 0 {
			return model.Sample{TrackID: 0, PTS: spec.TimeUS}
		}
		return video[idx]
	}
}

// countUnlocatable 统计无法定位到具体包序号/字节的故障数。
// 本流水线所有非致命故障都带轨道+序号（或截断带字节偏移），故正常为 0。
func countUnlocatable(items []model.Fault) int {
	n := 0
	for _, f := range items {
		hasSeq := f.Seq > 0 || f.Kind == fault.KindDrop
		hasBytes := f.ByteStart > 0 || f.ByteEnd > 0
		if !hasSeq && !hasBytes {
			n++
		}
	}
	return n
}

func ppmList(ds []timeline.DriftEstimate) []int64 {
	out := make([]int64, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.PPM)
	}
	return out
}
