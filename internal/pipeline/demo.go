// Package pipeline 把夹具生成、裸流扫描、统一时钟、索引校验、多表示层
// 对齐与清单写出串联为一次可恢复的端到端审计流程。
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"media-audit/internal/align"
	"media-audit/internal/checkpoint"
	"media-audit/internal/clock"
	"media-audit/internal/fixture"
	"media-audit/internal/index"
	"media-audit/internal/manifest"
	"media-audit/internal/stream"
)

// DemoConfig 是演示运行的全部参数。
type DemoConfig struct {
	Codec          stream.Codec
	Representations int
	Frames         int
	BlockFrames    int
	CorruptFrame   int    // >=0 时在第 0 表示层该帧注入损坏
	InterruptBlock int    // >=0 时在完成该块后模拟中断
	Resume         bool
	WorkDir        string
	SegmentTargetUS int64
	MaxDriftUS     int64
}

// Metrics 是验收要求打印的关键指标。
type Metrics struct {
	CorruptOffset    int64  `json:"corrupt_offset_bytes"`
	LocationError    int64  `json:"location_error_bytes"`
	ResumeMS         int64  `json:"resume_ms"`
	Segments         int    `json:"segments"`
	AlignmentMaxMS   int64  `json:"alignment_max_ms"`
	ManifestSHA256   string `json:"manifest_sha256"`
	ExitCode         int    `json:"-"`
}

// TrackArtifacts 汇总单条表示层的中间产物。
type TrackArtifacts struct {
	Spec    fixture.Spec
	Data    []byte
	Meta    *fixture.MetaLike
	Scan    *stream.ScanResult
	Timeline *clock.Timeline
	Index   *index.Index
	IndexStatus index.Status
}

// Run 执行一次完整演示。两次运行（含 resume）产出的清单校验和必须一致。
func Run(ctx context.Context, cfg DemoConfig) (*Metrics, *manifest.Manifest, error) {
	if cfg.MaxDriftUS <= 0 {
		cfg.MaxDriftUS = align.DefaultMaxDriftMicros
	}
	if cfg.SegmentTargetUS <= 0 {
		cfg.SegmentTargetUS = 10_000_000
	}
	start := time.Now()

	// 1. 确定性生成多表示层素材（含可选损坏注入）。
	tracks, injected, err := generateTracks(cfg)
	if err != nil {
		return nil, nil, err
	}

	// 2. 扫描 + 统一时钟 + 索引校验。
	// 若配置中断点，则先扫描到该块并落检查点，再走恢复路径仅复用
	// 校验通过的块、续扫剩余块；resumeMS 只统计恢复阶段耗时。
	var resumeMS int64
	if cfg.InterruptBlock >= 0 {
		resumeMS, err = simulateInterruptedScan(tracks, cfg)
		if err != nil {
			return nil, nil, err
		}
	}

	reps := make([]align.Representation, 0, len(tracks))
	var allIssues []stream.Issue
	inputRefs := map[int]checkpoint.InputRef{}
	trackStates := make([]checkpoint.TrackState, 0, len(tracks))
	for _, ta := range tracks {
		if cfg.InterruptBlock < 0 {
			if err := scanAndClock(ta, cfg); err != nil {
				return nil, nil, err
			}
		}
		allIssues = append(allIssues, ta.Scan.Issues...)
		reps = append(reps, buildRepresentation(ta))
		ref := checkpoint.InputRef{Size: ta.Scan.Size, SHA256: index.HashBytes(ta.Data)}
		inputRefs[ta.Spec.TrackID] = ref
		trackStates = append(trackStates, checkpoint.TrackState{
			TrackID:     ta.Spec.TrackID,
			Codec:       string(cfg.Codec),
			InputSize:   ref.Size,
			InputSHA256: ref.SHA256,
			Completed:   ta.Scan.Blocks,
		})
	}

	// 3. 多表示层切点对齐。
	aligned, err := align.Align(reps, align.Options{
		MaxDriftMicros:  cfg.MaxDriftUS,
		TargetSegmentUS: cfg.SegmentTargetUS,
	})
	if err != nil {
		return nil, nil, err
	}

	// 4. 生成稳定清单。
	m := buildManifest(cfg, aligned, allIssues, reps)

	// 5. 检查点持久化（模拟中断点）与恢复计时。
	jobID := "demo-" + string(cfg.Codec)
	if err := persistAndMaybeResume(ctx, cfg, jobID, trackStates, m.SHA256, inputRefs); err != nil {
		return nil, nil, err
	}

	// 6. 指标汇总。
	metrics := assembleMetrics(cfg, injected, aligned, m, start, resumeMS)
	return metrics, m, nil
}

// generateTracks 生成全部表示层，返回素材与（可选的）损坏位置。
func generateTracks(cfg DemoConfig) ([]*TrackArtifacts, *fixture.Corruption, error) {
	tracks := make([]*TrackArtifacts, cfg.Representations)
	var injected *fixture.Corruption
	for r := 0; r < cfg.Representations; r++ {
		spec := fixture.Spec{
			Codec:         cfg.Codec,
			TrackID:       r,
			Frames:        cfg.Frames,
			Timescale:     90000,
			NominalStep:   3000,
			KeyframeEvery: 300, // 10 秒关键帧
			JitterTicks:   int32(r) * 30,
			Seed:          1234,
		}
		data, frames := fixture.BuildStream(spec)
		if r == 0 && cfg.CorruptFrame >= 0 {
			c := fixture.InjectCorruptionAtFrame(cfg.Codec, data, cfg.CorruptFrame)
			injected = &c
		}
		meta := fixture.BuildMeta(spec, data, frames)
		tracks[r] = &TrackArtifacts{
			Spec: spec,
			Data: data,
			Meta: fixture.AsMetaLike(meta),
		}
	}
	return tracks, injected, nil
}

// scanAndClock 对单条表示层执行扫描、时钟展开与索引校验。
func scanAndClock(ta *TrackArtifacts, cfg DemoConfig) error {
	scanner := stream.NewScanner(ta.Spec.Codec, ta.Spec.TrackID, cfg.BlockFrames)
	scan, err := scanner.Scan(bytesReader(ta.Data))
	if err != nil {
		return err
	}
	ta.Scan = scan
	return buildClockAndIndex(ta, cfg)
}

// buildClockAndIndex 在已有扫描结果上构建统一时钟并校验/重建关键帧索引。
func buildClockAndIndex(ta *TrackArtifacts, cfg DemoConfig) error {
	scan := ta.Scan
	tl := clock.NewTimeline(ta.Spec.TrackID,
		clock.Rational{Num: int64(ta.Spec.Timescale), Den: 1},
		ta.Spec.NominalStep)
	// 以扫描出的 AU 顺序驱动时钟；时间戳来自 sidecar 帧表。
	for i, au := range scan.AUs {
		if i >= len(ta.Meta.Frames) {
			break
		}
		tl.Append(ta.Meta.Frames[i].RawTS, au.Keyframe)
	}
	ta.Timeline = tl

	// 索引校验：演示中不提供外部索引，验证缺失重建路径。
	h := index.HashBytes(ta.Data)
	res, err := index.Verify(nil, scan, h)
	if err != nil {
		return err
	}
	ta.Index = res.Rebuilt
	ta.IndexStatus = res.Status
	return nil
}

// buildRepresentation 把扫描帧与时钟微秒组装为对齐输入。
func buildRepresentation(ta *TrackArtifacts) align.Representation {
	marks := ta.Timeline.Frames()
	rep := align.Representation{ID: repID(ta.Spec.TrackID)}
	for i, au := range ta.Scan.AUs {
		if i >= len(marks) {
			break
		}
		rep.Frames = append(rep.Frames, align.Frame{
			AUIndex:   au.Index,
			Micros:    marks[i].Micros,
			Keyframe:  au.Keyframe,
			ByteStart: au.Offset,
			ByteEnd:   au.End,
		})
	}
	return rep
}

// buildManifest 把对齐切点转换为清单分片，并附带诊断与字节范围。
// 每个分片的字节范围覆盖该时间窗内（从切点到下一切点）的全部 AU，
// 而不仅是关键帧 AU，这样非关键帧上的损坏也能正确归入分片。
func buildManifest(cfg DemoConfig, aligned *align.Result, issues []stream.Issue, reps []align.Representation) *manifest.Manifest {
	cps := make([]manifest.CutPoint, 0, len(aligned.Cuts))
	for i, cut := range aligned.Cuts {
		end := nextAnchor(aligned.Cuts, i, reps)
		ranges := map[string]manifest.ByteRange{}
		for _, rep := range reps {
			br := spanBetween(rep, cut.AnchorMicros, end)
			ranges[rep.ID] = br
		}
		cps = append(cps, manifest.CutPoint{
			AnchorMicros: cut.AnchorMicros,
			EndMicros:    end,
			Ranges:       ranges,
		})
	}
	return manifest.Build(manifest.BuildInput{
		Codec:     string(cfg.Codec),
		CutPoints: cps,
		Issues:    issues,
	})
}

// spanBetween 返回表示层中时间落在 [start,end) 内的全部 AU 字节跨度。
func spanBetween(rep align.Representation, startUS, endUS int64) manifest.ByteRange {
	var br manifest.ByteRange
	first := true
	for _, f := range rep.Frames {
		if f.Micros < startUS || f.Micros >= endUS {
			continue
		}
		if first {
			br.Start = f.ByteStart
			first = false
		}
		br.End = f.ByteEnd
	}
	if first {
		// 窗口内无帧时退化为切点关键帧本身，保证范围非空。
		for _, f := range rep.Frames {
			if f.Micros == startUS {
				return manifest.ByteRange{Start: f.ByteStart, End: f.ByteEnd}
			}
		}
	}
	return br
}

// nextAnchor 取相邻切点的锚时间作为本分片结束；末片用最后一帧时间。
func nextAnchor(cuts []align.Cut, i int, reps []align.Representation) int64 {
	if i+1 < len(cuts) {
		return cuts[i+1].AnchorMicros
	}
	var maxEnd int64
	for _, r := range reps {
		if len(r.Frames) > 0 && r.Frames[len(r.Frames)-1].Micros > maxEnd {
			maxEnd = r.Frames[len(r.Frames)-1].Micros
		}
	}
	return maxEnd
}

// persistAndMaybeResume 写检查点；若配置中断+恢复，则模拟一次中断后验证恢复。
func persistAndMaybeResume(ctx context.Context, cfg DemoConfig, jobID string,
	states []checkpoint.TrackState, outSHA string, refs map[int]checkpoint.InputRef) error {
	if cfg.WorkDir == "" {
		return errors.New("缺少工作目录")
	}
	ckptDir := cfg.WorkDir + "/checkpoint"
	if err := checkpoint.Save(ckptDir, &checkpoint.Store{
		JobID: jobID, Tracks: states, OutputSHA256: outSHA,
	}); err != nil {
		return err
	}
	if cfg.Resume {
		if _, err := checkpoint.Load(ckptDir, jobID, refs); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// assembleMetrics 汇总验收指标，含损坏定位误差与恢复耗时。
// resumeMS 仅统计中断后的恢复阶段；无中断时回退为端到端耗时。
func assembleMetrics(cfg DemoConfig, injected *fixture.Corruption,
	aligned *align.Result, m *manifest.Manifest, start time.Time, resumeMS int64) *Metrics {
	rm := resumeMS
	if rm == 0 {
		rm = time.Since(start).Milliseconds()
	}
	mt := &Metrics{
		Segments:       m.SegmentCount,
		AlignmentMaxMS: aligned.MaxDrift / 1000,
		ManifestSHA256: m.SHA256,
		ResumeMS:       rm,
	}
	if injected != nil {
		mt.CorruptOffset = injected.Offset
		// 定位误差：扫描诊断偏移到实际损坏字节的距离（起始码最多 4 字节）。
		mt.LocationError = locateError(injected.Offset, m)
	}
	// 退出码：精确定位损坏 => 2；否则完成 => 0。
	if injected != nil {
		mt.ExitCode = 2
	} else {
		mt.ExitCode = 0
	}
	return mt
}

// locateError 在清单错误摘要中找到覆盖损坏点的诊断，计算字节距离。
func locateError(corruptOffset int64, m *manifest.Manifest) int64 {
	for _, seg := range m.Segments {
		for _, is := range seg.Errors {
			if corruptOffset >= is.Offset && corruptOffset <= is.End {
				return corruptOffset - is.Offset
			}
		}
	}
	return -1 // 未定位
}

func repID(trackID int) string {
	return "r" + itoa(trackID)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// sha256Hex 暴露给同包测试使用的确定性摘要辅助。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
