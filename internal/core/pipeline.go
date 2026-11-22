package core

import (
	"context"

	"mediacheck/internal/adapter"
	"mediacheck/internal/adapter/audio"
	mp4adapter "mediacheck/internal/adapter/mp4"
	"mediacheck/internal/fixture"
	"mediacheck/internal/manifest"
	"mediacheck/internal/model"
	"mediacheck/internal/report"
)

// PipelineConfig 是演示/验收的端到端参数。
type PipelineConfig struct {
	Seed          int64
	Rate          uint32 // 输入素材采样率（44100/48000）
	Layout        model.ChannelLayout
	TargetRate    uint32
	TargetLayout  model.ChannelLayout
	TargetLUFS    float64
	PeakLimit     float64
	TargetKBPS    int
	Injection     fixture.Injection
	Policy        RegressionPolicy
	OutDir        string
	ColorPriority ColorPriority
	FillSilence   bool
}

// PipelineResult 聚合最终指标与清单路径（成功时）。
type PipelineResult struct {
	Metrics      report.Metrics
	ManifestPath string
	Rejected     bool
}

// Run 执行完整数据流：夹具→注册表解析→时钟归一化→PCM 适配→音频内核→
// 色彩仲裁→再封装→清单对齐→报告摘要。任何阶段的结构化错误都向上透传。
func Run(ctx context.Context, reg *adapter.Registry, cfg PipelineConfig) (*PipelineResult, error) {
	bundle := fixture.Generate(cfg.Seed, cfg.Rate, cfg.Layout, 4, cfg.Injection)
	parser, err := reg.Lookup("mp4", 1)
	if err != nil {
		return nil, err
	}
	parsed, err := parser.Parse(ctx, adapter.ParseInput{
		InitSegment: bundle.InitSegment,
		Fragments:   bundle.Fragments,
	})
	if err != nil {
		return rejectOrError(cfg, err)
	}
	// 未知布局：在进入音频内核前按轨道号拒绝。
	var audioTrack model.TrackMeta
	for _, tm := range parsed.Tracks {
		if tm.Codec == "ipcm" {
			audioTrack = tm
		}
	}
	if audioTrack.Layout == "" {
		return rejectOrError(cfg, &model.MediaError{
			Category: model.ErrUnknownLayout, TrackID: audioTrack.TrackID,
			Message: "unknown channel layout in track",
		})
	}
	// 时钟内核：缺口、回退、去重。
	clock, err := NormalizeTimeline(parsed.Fragments, cfg.Policy)
	if err != nil {
		return rejectOrError(cfg, err)
	}
	// PCM 解码（按归一化后的分片顺序汇总样本）。
	dec, err := audio.NewDecoder(audioTrack)
	if err != nil {
		return rejectOrError(cfg, err)
	}
	var frames []audio.Frame
	var decodeStats audio.DecodeStats
	for _, f := range clock.Ordered {
		for _, s := range f.Samples {
			fr, st, err := dec.Decode(s.Data)
			if err != nil {
				return rejectOrError(cfg, err)
			}
			frames = append(frames, fr...)
			decodeStats.SilentFrames += st.SilentFrames
			decodeStats.ClippedSamples += st.ClippedSamples
			decodeStats.TotalSamples += st.TotalSamples
		}
	}
	// 音频内核统一规格。
	norm, err := NormalizeTrack(NormalizeConfig{
		InRate: audioTrack.SampleRate, TargetRate: cfg.TargetRate,
		InLayout: audioTrack.Layout, TargetLayout: cfg.TargetLayout,
		TargetLUFS: cfg.TargetLUFS, PeakLimit: cfg.PeakLimit,
		FillSilence: cfg.FillSilence, TrackID: audioTrack.TrackID,
	}, frames)
	if err != nil {
		return rejectOrError(cfg, err)
	}
	// 色彩仲裁（来源取自所有轨道的 colr）。
	var colorSrcs []*model.ColorMeta
	for _, tm := range parsed.Tracks {
		if tm.Color != nil {
			colorSrcs = append(colorSrcs, tm.Color)
		}
	}
	color := ResolveColor(colorSrcs, cfg.ColorPriority)
	// 再封装：输出分片沿用时钟时间线的原始 mfhd 序号，保证清单可对齐。
	var packSeqs []uint32
	for _, iv := range clock.Timeline {
		packSeqs = append(packSeqs, iv.FragmentSeq)
	}
	pack, err := Pack(PackInput{
		OutDir: cfg.OutDir, BaseName: "program", Timescale: cfg.TargetRate,
		TargetKBPS: cfg.TargetKBPS, FramesPerFrag: int(cfg.TargetRate),
		PCM16: norm.PCM16, Channels: cfg.TargetLayout.Channels(),
		Timeline: clock.Timeline, Seqs: packSeqs, Color: color.Selected,
	})
	if err != nil {
		return rejectOrError(cfg, err)
	}
	// 清单：分片文件映射（只取 .m4s 媒体分片，排除 init 与 free box）。
	files := map[uint32]string{}
	for i, seq := range pack.FragmentSeqs {
		name := "program-" + pad6(seq) + ".m4s"
		path, perr := adapter.SafeOutputPath(cfg.OutDir, name)
		if perr != nil {
			return rejectOrError(cfg, perr)
		}
		files[seq] = path
		_ = i
	}
	mf, err := manifest.Build(clock.Timeline, files, cfg.TargetRate, cfg.TargetRate,
		string(cfg.TargetLayout), 1.0)
	if err != nil {
		return rejectOrError(cfg, err)
	}
	manifestPath, err := mf.Write(cfg.OutDir, "manifest.json")
	if err != nil {
		return rejectOrError(cfg, err)
	}
	metrics := report.Build(report.Inputs{
		Seed: cfg.Seed, TargetKBPS: cfg.TargetKBPS,
		Normalize: model.NormalizeSnapshot{
			TrackID: norm.TrackID, InRate: norm.InRate, OutRate: norm.OutRate,
			SamplesIn: norm.SamplesIn, SamplesOut: norm.SamplesOut, FrameDelta: norm.FrameDelta,
			PeakDBFS: norm.PeakDBFS, IntegratedLUFS: norm.Stats.IntegratedLUFS,
			SilentFrames: norm.Stats.SilentFrames, ClippedSamples: norm.Stats.ClippedSamples,
			ShortFrames: norm.Stats.ShortFrames,
		},
		Pack: model.PackSnapshot{
			ActualKBPS: pack.ActualKBPS, MediaBytes: pack.MediaBytes, FreeBytes: pack.FreeBytes,
		},
		Color: model.ColorSnapshot{Selected: color.Selected, Conflicts: color.Conflicts},
		AlignMS: mf.AlignmentMaxMS, Gaps: len(clock.Gaps),
		UnknownBoxes: countUnknown(parsed.Boxes),
	})
	return &PipelineResult{Metrics: metrics, ManifestPath: manifestPath}, nil
}

func rejectOrError(cfg PipelineConfig, err error) (*PipelineResult, error) {
	if me, ok := model.AsMediaError(err); ok {
		return &PipelineResult{
			Rejected: true,
			Metrics:  report.Reject(cfg.Seed, me.Category),
		}, err
	}
	return nil, err
}

func countUnknown(boxes []model.BoxSummary) int {
	n := 0
	for _, b := range boxes {
		if b.Unknown {
			n++
		}
	}
	return n
}

func pad6(seq uint32) string {
	const digits = "0123456789"
	out := make([]byte, 6)
	for i := 5; i >= 0; i-- {
		out[i] = digits[seq%10]
		seq /= 10
	}
	return string(out)
}

// DefaultRegistry 注册内置 mp4 适配器及其声明的 codec。
func DefaultRegistry() (*adapter.Registry, error) {
	reg := adapter.NewRegistry()
	if err := reg.Register(mp4adapter.New(), mp4adapter.SupportedCodecsList()); err != nil {
		return nil, err
	}
	return reg, nil
}
