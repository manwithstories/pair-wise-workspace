package pipeline

import (
	"medpipe/internal/clock"
	"medpipe/internal/manifest"
	"medpipe/internal/quality"
	"medpipe/internal/render"
)

// finalize 把合轨 PCM 渲染为帧序列，搜索目标码率并写出清单。
func (r *Runner) finalize(cp *Checkpoint, res *Result) (*Result, error) {
	pcm := r.mixer.PCM()
	tracks, _, err := render.Decode16Bit(pcm, r.mixer.Channels())
	if err != nil {
		return nil, err
	}
	frameCount := r.tb.FrameCount(r.mixer.TotalFrames(), r.fps)
	wantFrames := int64(r.opts.Seconds) * r.opts.FPSNum / r.opts.FPSDen
	if frameCount != wantFrames {
		return nil, clock.NewError(clock.CodeInvalidParam, -1,
			"pipeline: 帧数 %d 与 fps/时长几何不符(应 %d)", frameCount, wantFrames)
	}
	// 单帧目标字节：目标码率 / 8 / fps。
	targetBytes := r.opts.TargetBPS * r.opts.FPSDen / (8 * r.opts.FPSNum)
	metrics := make([]quality.FrameMetric, 0, frameCount)
	for i := int64(0); i < frameCount; i++ {
		start := r.tb.FrameStart(i, r.fps)
		end := r.tb.FrameStart(i+1, r.fps)
		if end > r.mixer.TotalFrames() {
			end = r.mixer.TotalFrames()
		}
		img := render.WaveformImage(tracks, start, end, int(i))
		q, jpegBytes, ref, compressed, err := render.SearchQuality(img, int(targetBytes))
		if err != nil {
			return nil, err
		}
		_ = q
		fm, err := render.RenderFrame(r.opts.OutDir, i, ref, compressed, jpegBytes)
		if err != nil {
			return nil, err
		}
		metrics = append(metrics, fm)
	}
	clipped, missing, silenceFrames, maxAlign, ranges := r.mixer.Stats()
	// compensated_frames 涵盖网络丢包与块截断两类静音补偿（按采样帧计）。
	compensated := silenceFrames
	for _, b := range r.blocks {
		if b.Truncated {
			compensated += b.MissingFrames
		}
	}
	sum := quality.Finalize(metrics, r.opts.TargetBPS, r.opts.FPSNum, r.opts.FPSDen,
		clipped, missing, maxAlign, compensated)
	cp.Interrupted = false
	if err := cp.save(r.opts.OutDir); err != nil {
		return nil, err
	}
	mf := manifest.Build(r.opts.Seed, r.tb.Rate, r.mixer.Channels(), r.fps, string(r.opts.Fault),
		metrics, sum, ranges, r.blocks, cp.Chain)
	path, err := mf.Write(r.opts.OutDir)
	if err != nil {
		return nil, err
	}
	res.Summary = sum
	res.Manifest = mf
	res.Checkpoint = cp
	_ = path
	return res, nil
}
