package pipeline

import (
	"strconv"

	"medpipe/internal/clock"
	"medpipe/internal/index"
	"medpipe/internal/ingest"
	"medpipe/internal/manifest"
	"medpipe/internal/quality"
	"medpipe/internal/timeline"
)

// Options 是一次流水线运行的完整参数。
type Options struct {
	Seed         int64
	Tracks       int
	Seconds      int
	FPSNum       int64
	FPSDen       int64
	TargetBPS    int64
	OutDir       string
	InputDir     string
	Fault        ingest.Fault
	StopAfter    int // >0 时处理完该数量的块后保留检查点中断
	Resume       bool
	MaxBlock     int64
	WindowSize   int64
	PacketFrames int64
}

// Result 汇总运行产出，供 CLI 打印验收字段。
type Result struct {
	Summary     quality.Summary
	Manifest    *manifest.Manifest
	Interrupted bool
	Checkpoint  *Checkpoint
	Network     ingest.NetworkStats
}

// Runner 编排所有子系统。
type Runner struct {
	opts   Options
	tb     clock.TimeBase
	fps    clock.Rational
	mixer  *timeline.Mixer
	blocks []manifest.BlockRecord
	window int64
}

// New 校验参数并构造 Runner。
func New(opts Options) (*Runner, error) {
	if opts.Tracks <= 0 || opts.Seconds <= 0 || opts.FPSNum <= 0 || opts.TargetBPS <= 0 {
		return nil, clock.NewError(clock.CodeInvalidParam, -1, "pipeline: 非法运行参数")
	}
	if opts.MaxBlock <= 0 {
		opts.MaxBlock = index.DefaultMaxBlock
	}
	if opts.WindowSize <= 0 {
		opts.WindowSize = 8
	}
	if opts.PacketFrames <= 0 {
		opts.PacketFrames = 480
	}
	tb, err := clock.NewTimeBase(48000)
	if err != nil {
		return nil, err
	}
	fps, err := clock.NewRational(opts.FPSNum, opts.FPSDen)
	if err != nil {
		return nil, err
	}
	return &Runner{opts: opts, tb: tb, fps: fps}, nil
}

func (r *Runner) params() map[string]string {
	return map[string]string{
		"seed":    strconv.FormatInt(r.opts.Seed, 10),
		"tracks":  strconv.Itoa(r.opts.Tracks),
		"seconds": strconv.Itoa(r.opts.Seconds),
		"fps":     r.fps.String(),
		"target":  strconv.FormatInt(r.opts.TargetBPS, 10),
		"fault":   string(r.opts.Fault),
		"packets": strconv.FormatInt(r.opts.PacketFrames, 10),
	}
}
