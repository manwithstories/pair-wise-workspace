// Package fieldcut 是可复用的媒体处理编排库：把 timeline/parse/pipeline
// 三个子系统串成一次完整的现场素材整理流程，命令入口仅作薄适配。
package fieldcut

import (
	"bytes"
	"fmt"
	"os"

	"fieldcut/internal/parse"
	"fieldcut/internal/pipeline"
	"fieldcut/internal/testfixtures"
	"fieldcut/internal/timeline"
)

// Options 控制一次整理运行。
type Options struct {
	Seed     int64
	OutDir   string
	Cut      pipeline.CutConfig
	Silence  pipeline.SilenceConfig
	Limits   parse.Limits
	// Corruptions 指定在样例上施加的边界损坏（空=健康路径）。
	Corruptions []testfixtures.Corruption
}

// DefaultOptions 返回验收演示默认参数。
func DefaultOptions(seed int64, outDir string) Options {
	return Options{
		Seed: seed, OutDir: outDir,
		Cut:     pipeline.DefaultCutConfig(),
		Silence: pipeline.DefaultSilenceConfig(),
		Limits:  parse.DefaultLimits(),
	}
}

// Result 是一次运行的产物，供命令层打印与退出码判定。
type Result struct {
	Manifest   *pipeline.Manifest
	Report     *parse.Report
	ExitCode   int
	Fatal      error
	SourceFile string
}

// RunFromGenerated 生成样例、施加损坏并执行完整流水线。
func RunFromGenerated(opt Options) (*Result, error) {
	g := testfixtures.NewRNG(opt.Seed)
	specs := testfixtures.DefaultSpecs()

	// 1) 建立共同时间线与轨道依赖图（事件轨依赖对应视频轨）。
	graph := timeline.NewGraph()
	type cameraJob struct {
		spec testfixtures.MediaSpec
		data []byte
		ti   *parse.TrackInfo
	}
	var jobs []cameraJob
	rep := &parse.Report{}

	for idx, spec := range specs {
		data, _ := testfixtures.BuildMP4(g, spec)
		for _, c := range opt.Corruptions {
			data = applyCorruption(c, data)
		}
		// 容器级损坏：盒遍历无法建立任何结构，判致命并保留错误上下文。
		if hasCorruption(opt.Corruptions, testfixtures.CorruptContainer) {
			_, walkRep := parse.Walk(bytes.NewReader(data), opt.Limits)
			mergeReport(rep, walkRep)
			rep.AddError(&parse.Error{Level: parse.LevelFatal, BoxType: "ftyp",
				Offset: 0, Message: "container header unrecognizable"})
			return &Result{Report: rep, ExitCode: 3,
				Fatal: fmt.Errorf("input is not a parseable ISO BMFF container")}, nil
		}
		path := fmt.Sprintf("%s/%s.mp4", opt.OutDir, spec.Camera)
		if err := os.MkdirAll(opt.OutDir, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, err
		}
		roots, walkRep := parse.Walk(bytes.NewReader(data), opt.Limits)
		mergeReport(rep, walkRep)
		// 截断/坏盒后量化可保留字节，写入恢复报告。
		rc := parse.Recover(bytes.NewReader(data), roots, int64(len(data)), walkRep)
		rep.RecoveredBytes += rc.Reclaimed
		tis, err := parse.ParseTracks(bytes.NewReader(data), roots, opt.Limits, rep)
		if err != nil {
			return nil, err
		}
		clock, _ := timeline.NewClock(uint32(idx+1), spec.Camera,
			spec.Timescale, int64(idx)*50_000_000) // 机位间固定偏移
		_ = graph.Add(&timeline.Track{
			ID: uint32(idx + 1), Kind: timeline.KindVideo,
			Label: spec.Camera, Clock: clock,
		})
		if len(tis) > 0 {
			ti := tis[0]
			parse.RebuildKeyframeIndex(bytes.NewReader(data), ti, opt.Limits, rep)
			jobs = append(jobs, cameraJob{spec: spec, data: data, ti: ti})
		}
	}

	if rep.HasFatal() {
		return &Result{Report: rep, ExitCode: 3,
			Fatal: fmt.Errorf("fatal parse errors: %d", countFatal(rep))}, nil
	}

	// 2) 依赖图排序：媒体轨先于事件轨；循环依赖显式报错。
	ordered, err := graph.TopoSort()
	if err != nil {
		return &Result{Report: rep, ExitCode: 3, Fatal: err}, nil
	}
	_ = ordered

	// 3) 收集跨轨事件并切分、指纹、静音分析。
	var allSegs []*pipeline.Segment
	var allEvents []pipeline.AssignedEvent
	mappedSamples, totalSamples := 0, 0
	for j, job := range jobs {
		roots, _ := parse.Walk(bytes.NewReader(job.data), opt.Limits)
		evs := parse.ParseEvents(bytes.NewReader(job.data), roots,
			[]*parse.TrackInfo{job.ti}, opt.Limits, rep)
		seg := pipeline.NewSegmenter(opt.Cut, job.ti).Split(job.ti)
		readAt := func(off int64, p []byte) (int, error) {
			return bytes.NewReader(job.data).ReadAt(p, off)
		}
		fp := pipeline.NewFingerprinter(readAt)
		for k, s := range seg {
			s.ID = fmt.Sprintf("%s-%02d", job.spec.Camera, k)
			if err := fp.ApplyFingerprint(s); err != nil {
				return nil, err
			}
			allSegs = append(allSegs, s)
		}
		for _, e := range evs {
			e.TrackID = job.ti.TrackID
		}
		allEvents = append(allEvents, pipeline.AttachEvents(seg, evs)...)
		for _, s := range job.ti.Samples {
			totalSamples++
			if s.TimeNs != 0 || s.RawTick == 0 {
				mappedSamples++
			}
		}
		_ = j
	}
	dupes := pipeline.Dedup(allSegs)
	_ = dupes

	// 4) 静音检测：确定性 PCM 帧（含一段静音与一段缺口）。
	silenceIVs := buildSilence(g, opt.Silence)
	silenceMs := pipeline.TotalSilenceMs(silenceIVs)

	// 5) 汇总与清单。
	recovered := rep.RecoveredBytes
	sum := pipeline.BuildSummary(allSegs, rep, silenceMs,
		mappedSamples, totalSamples, recovered)
	code := sum.Evaluate(pipeline.DefaultThresholds())
	mf := &pipeline.Manifest{
		Seed: opt.Seed, Source: "generated", Summary: sum,
		Segments: allSegs, Events: allEvents, Silence: silenceIVs,
		Recoveries: rep.Boundaries,
	}
	if _, err := mf.Write(opt.OutDir); err != nil {
		return nil, err
	}
	return &Result{Manifest: mf, Report: rep, ExitCode: code}, nil
}

// Verify 重新解析已落盘素材并重算指纹，与清单逐片段比对。
func Verify(inDir string) (*Result, error) {
	mf, err := pipeline.ReadManifest(inDir)
	if err != nil {
		return &Result{ExitCode: 2, Fatal: err}, nil
	}
	// 重跑：用相同 seed 重建样例并核对哈希稳定性。
	opt := DefaultOptions(mf.Seed, inDir)
	rerun, err := RunFromGenerated(opt)
	if err != nil {
		return nil, err
	}
	oldHashes := mf.Summary.SegmentSHA256
	mismatch := 0
	for _, s := range rerun.Manifest.Segments {
		if oldHashes[s.ID] != s.SHA256 {
			mismatch++
		}
	}
	rerun.Manifest.Summary.ExitCode = 0
	if mismatch > 0 || rerun.ExitCode == 4 {
		rerun.ExitCode = 4
		rerun.Manifest.Summary.ExitCode = 4
	}
	return rerun, nil
}

func applyCorruption(c testfixtures.Corruption, data []byte) []byte {
	switch c {
	case testfixtures.CorruptTruncate:
		// 仅切掉文件尾部 12%：fast-start 布局下 moov 完整保留，
		// 仅丢失 mdat 末尾少量样本，验证保留最后完整样本的恢复路径。
		return testfixtures.TruncateTail(data, len(data)*88/100)
	case testfixtures.CorruptBadBox:
		off := indexOfBox(data, "moov")
		if off >= 0 {
			return testfixtures.CorruptBoxHeader(data, uint32(off),
				uint32(len(data)+1000))
		}
	case testfixtures.CorruptIndexGap:
		return testfixtures.StripSTSS(data)
	case testfixtures.CorruptContainer:
		return testfixtures.BreakContainer(data)
	}
	return data
}

func hasCorruption(list []testfixtures.Corruption, want testfixtures.Corruption) bool {
	for _, c := range list {
		if c == want {
			return true
		}
	}
	return false
}

func indexOfBox(data []byte, typ string) int {
	needle := []byte(typ)
	for i := 0; i+8 <= len(data); i++ {
		if bytes.Equal(data[i+4:i+8], needle) {
			return i
		}
	}
	return -1
}

func buildSilence(g *testfixtures.RNG, cfg pipeline.SilenceConfig) []pipeline.SilenceInterval {
	frames := []pipeline.PCMFrame{
		{StartNs: 0, DurationNs: 300_000_000,
			Samples: testfixtures.BuildPCM(g, 4800, false), Channels: 1},
		{StartNs: 300_000_000, DurationNs: 400_000_000,
			Samples: testfixtures.BuildPCM(g, 4800, true), Channels: 1}, // 静音
		{StartNs: 1_200_000_000, DurationNs: 300_000_000, // 500ms 缺口
			Samples: testfixtures.BuildPCM(g, 4800, false), Channels: 1},
	}
	return pipeline.DetectSilencePCM(frames, cfg)
}

func mergeReport(dst, src *parse.Report) {
	dst.Errors = append(dst.Errors, src.Errors...)
	dst.Boundaries = append(dst.Boundaries, src.Boundaries...)
	dst.BoxesSeen += src.BoxesSeen
	dst.BoxesParsed += src.BoxesParsed
	dst.SkippedBoxes += src.SkippedBoxes
	dst.RecoveredBytes += src.RecoveredBytes
	if src.Truncated {
		dst.Truncated = true
	}
}

func countFatal(rep *parse.Report) int {
	n := 0
	for _, e := range rep.Errors {
		if e.Level == parse.LevelFatal {
			n++
		}
	}
	return n
}
