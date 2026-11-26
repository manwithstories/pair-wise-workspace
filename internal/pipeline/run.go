// Package pipeline 流程编排：清单解析→哈希校验→时间线→合成→封装→输出。
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"image"
	"path/filepath"

	"aircheck/internal/adapter"
	"aircheck/internal/checkpoint"
	"aircheck/internal/pack"
	"aircheck/internal/render"
)

// ExitError 带进程退出码的错误（严格模式坏哈希为 2）。
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// ErrInterrupted 运行被截断（检查点已保存，可续跑）。
var ErrInterrupted = errors.New("pipeline: interrupted, checkpoint saved")

// Options 运行参数。
type Options struct {
	ManifestPath   string
	SourceRoot     string // 源帧根目录
	OutDir         string // 输出仅写入此目录
	TargetKbps     float64
	Strict         bool // 坏哈希帧立即拒绝（退出码 2）
	Resume         bool // 从检查点续跑
	InterruptAfter int  // >0：写出 N 帧后模拟中断（demo 用）
	Width, Height  int
	FPS            float64 // 输出帧率，0 = 最高优先级轨帧率
	MaxBytes       int64
}

// Result 运行结果。
type Result struct {
	Frames     int
	GapFrames  int
	BadFrames  int
	BadRanges  []render.BadRange
	AlignMaxMs float64
	Rate       *pack.Result
	OutputSHA  string
	Resumed    bool
}

// Run 执行完整流水线。
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.ManifestPath == "" || opts.SourceRoot == "" || opts.OutDir == "" {
		return nil, fmt.Errorf("pipeline: manifest, source root and out dir are required")
	}
	if opts.TargetKbps <= 0 {
		return nil, fmt.Errorf("pipeline: target kbps must be positive")
	}
	if opts.Width <= 0 {
		opts.Width = 320
	}
	if opts.Height <= 0 {
		opts.Height = 180
	}
	mData, err := adapter.ReadSecure(filepath.Dir(opts.ManifestPath), filepath.Base(opts.ManifestPath), opts.MaxBytes)
	if err != nil {
		return nil, err
	}
	m, err := adapter.ParseManifest(opts.ManifestPath, opts.MaxBytes)
	if err != nil {
		return nil, err
	}
	dg := digests{
		manifest: sha256Hex(mData),
		params: checkpoint.Digest("params-v1",
			fmt.Sprintf("%g|%g|%d|%d", opts.TargetKbps, opts.FPS, opts.Width, opts.Height)),
	}
	badIdx, err := verifyHashes(ctx, opts.SourceRoot, m, opts.MaxBytes)
	if err != nil {
		return nil, err
	}
	if len(badIdx) > 0 && opts.Strict {
		return nil, &ExitError{Code: 2, Err: fmt.Errorf("strict: %d bad-hash frame(s) rejected", len(badIdx))}
	}
	dg.sources = sourcesDigest(m)
	tracks, gapFrames, err := buildTracks(m, badIdx)
	if err != nil {
		return nil, err
	}
	outFPS := opts.FPS
	if outFPS <= 0 {
		outFPS = tracks[0].FPS
	}
	plan, err := render.Compose(tracks, outFPS)
	if err != nil {
		return nil, err
	}
	if len(plan.Frames) > maxOutFrames {
		return nil, fmt.Errorf("pipeline: %d output frames exceeds limit %d", len(plan.Frames), maxOutFrames)
	}
	loader := func(file string) (image.Image, error) {
		img, _, err := adapter.LoadImage(opts.SourceRoot, file, opts.MaxBytes)
		return img, err
	}
	imgs, err := render.Render(plan, loader, opts.Width, opts.Height)
	if err != nil {
		return nil, err
	}
	durationMs := float64(len(imgs)) * 1000 / outFPS
	rate, err := pack.RateControl(imgs, opts.TargetKbps, durationMs)
	if err != nil {
		return nil, err
	}
	frameSHAs := make([]string, len(rate.Frames))
	for i, bs := range rate.Frames {
		frameSHAs[i] = sha256Hex(bs)
	}
	res := &Result{Frames: len(imgs), GapFrames: gapFrames, BadFrames: len(badIdx),
		BadRanges: plan.BadRanges, AlignMaxMs: plan.AlignMaxMs, Rate: rate,
		OutputSHA: checkpoint.Digest(frameSHAs...)}
	resumed, err := writeOutputs(ctx, opts, m, plan, rate, frameSHAs, dg, res.OutputSHA)
	if err != nil {
		return nil, err
	}
	res.Resumed = resumed
	if err := writeStats(opts.OutDir, res); err != nil {
		return nil, err
	}
	return res, nil
}
