package core

import (
	"mediacheck/internal/adapter/audio"
	"mediacheck/internal/model"
)

// NormalizeResult 是音频内核处理一轨后的完整结果，供再封装与报告消费。
type NormalizeResult struct {
	TrackID        uint32
	InRate         uint32
	OutRate        uint32
	InLayout       model.ChannelLayout
	OutLayout      model.ChannelLayout
	FramesIn       int
	FramesOut      int
	SamplesIn      int64
	SamplesOut     int64
	ExpectedOut    int64 // 有理数核算的期望输出帧数
	FrameDelta     int64 // 实际与期望的帧差，绝对值 ≤1
	PCM16          []byte
	Stats          LoudnessStats
	PeakDBFS       float64
	GainQ15        int64
}

// NormalizeTrack 对一轨 PCM 帧序列执行完整音频规格统一：
// 重采样（按声道）→ 声道矩阵 → 响度测量 → 目标 LUFS 增益与峰值限制 → 编码 16-bit。
func NormalizeTrack(cfg NormalizeConfig, frames []audio.Frame) (*NormalizeResult, error) {
	if !model.SupportedRate(cfg.InRate) || !model.SupportedRate(cfg.TargetRate) {
		return nil, &model.MediaError{Category: model.ErrUnsupportedRate, TrackID: cfg.TrackID}
	}
	matrix, err := BuildMatrix(cfg.InLayout, cfg.TargetLayout, cfg.FillSilence)
	if err != nil {
		if me, ok := model.AsMediaError(err); ok {
			me.TrackID = cfg.TrackID
		}
		return nil, err
	}
	rs, err := NewResampler(cfg.InRate, cfg.TargetRate)
	if err != nil {
		return nil, err
	}
	outCh := cfg.TargetLayout.Channels()
	meter := NewLoudnessMeter(outCh, cfg.TargetRate)
	limiter := NewLimiter(cfg.TargetLUFS, cfg.PeakLimit)

	var samplesIn int64
	// 第一步：矩阵混音（在原始采样率上），同时统计输入样本。
	mixed := make([][]int32, 0, len(frames))
	for _, fr := range frames {
		samplesIn += int64(len(fr.Samples))
		m, err := matrix.Apply(fr.Samples, cfg.TrackID)
		if err != nil {
			return nil, err
		}
		mixed = append(mixed, m)
	}
	// 第二步：逐声道重采样到目标采样率。
	resampled := make([][]int32, 0, len(mixed))
	perCh := make([][]int32, outCh)
	for c := 0; c < outCh; c++ {
		rs.Reset()
		mono := make([]int32, len(mixed))
		for i, fr := range mixed {
			mono[i] = fr[c]
		}
		perCh[c] = rs.Process(mono)
	}
	framesOut := len(perCh[0])
	for i := 0; i < framesOut; i++ {
		fr := make([]int32, outCh)
		for c := 0; c < outCh; c++ {
			fr[c] = perCh[c][i]
		}
		resampled = append(resampled, fr)
	}
	// 第三步：先测量原始响度，再计算增益并限幅。
	for _, fr := range resampled {
		meter.Push(fr)
	}
	stats := meter.Finalize()
	limiter.ComputeGain(stats)
	final := make([]audio.Frame, 0, framesOut)
	for _, fr := range resampled {
		out, _ := limiter.Apply(fr)
		final = append(final, audio.Frame{Samples: out})
	}
	pcm := audio.Encode16(final, outCh)
	expected := ExpectedOutputFrames(len(frames), cfg.InRate, cfg.TargetRate)
	delta := int64(framesOut) - expected
	if delta < 0 {
		delta = -delta
	}
	return &NormalizeResult{
		TrackID: cfg.TrackID, InRate: cfg.InRate, OutRate: cfg.TargetRate,
		InLayout: cfg.InLayout, OutLayout: cfg.TargetLayout,
		FramesIn: len(frames), FramesOut: framesOut,
		SamplesIn: samplesIn, SamplesOut: int64(framesOut) * int64(outCh),
		ExpectedOut: expected, FrameDelta: delta,
		PCM16: pcm, Stats: stats, PeakDBFS: limiter.PeakDBFS(), GainQ15: limiter.gainQ15,
	}, nil
}
