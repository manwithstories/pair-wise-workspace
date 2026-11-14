package job

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"rcx/internal/codec"
	"rcx/internal/rate"
	"rcx/internal/reel"
	"rcx/internal/report"
	"rcx/internal/task"
)

var (
	ErrRateDeviation = errors.New("rate deviation exceeds 2 percent")
	ErrInterrupted   = errors.New("job interrupted and can be resumed")
	ErrRegression    = errors.New("quality regression detected")
)

type EncodeOptions struct {
	ReelPath          string
	TargetBPS         int64
	MinQP             int
	MaxQP             int
	Concurrency       int
	RetryMax          int
	CrashAfterSegment int
	JobDir            string
}

type EncodeResult struct {
	Report      report.Report
	Skipped     int
	Reencoded   int
	Interrupted bool
}

func RunEncode(ctx context.Context, opts EncodeOptions) (EncodeResult, error) {
	if opts.JobDir == "" || opts.ReelPath == "" || opts.TargetBPS <= 0 || opts.MinQP < 0 || opts.MaxQP > 63 || opts.MinQP > opts.MaxQP || opts.Concurrency < 1 || opts.RetryMax < 0 || opts.CrashAfterSegment < 0 {
		return EncodeResult{}, errors.New("invalid encode options")
	}
	paths := NewPaths(opts.JobDir)
	if info, err := os.Stat(paths.Journal); err == nil && info.Size() > 0 {
		return EncodeResult{}, fmt.Errorf("job %s already exists; use resume", opts.JobDir)
	}
	absReel, err := filepath.Abs(opts.ReelPath)
	if err != nil {
		return EncodeResult{}, err
	}
	cfg := Config{
		Version:           configVersion,
		ReelPath:          absReel,
		TargetBPS:         opts.TargetBPS,
		MinQP:             opts.MinQP,
		MaxQP:             opts.MaxQP,
		Concurrency:       opts.Concurrency,
		RetryMax:          opts.RetryMax,
		CrashAfterSegment: opts.CrashAfterSegment,
	}
	if err := paths.Prepare(); err != nil {
		return EncodeResult{}, err
	}
	if err := writeJSONAtomic(paths.Config, cfg); err != nil {
		return EncodeResult{}, err
	}
	return execute(ctx, paths, cfg, false)
}

func RunResume(ctx context.Context, jobDir string) (EncodeResult, error) {
	paths := NewPaths(jobDir)
	cfg, err := loadConfig(paths.Config)
	if err != nil {
		return EncodeResult{}, err
	}
	cfg.CrashAfterSegment = 0
	return execute(ctx, paths, cfg, true)
}

func execute(ctx context.Context, paths Paths, cfg Config, resume bool) (EncodeResult, error) {
	if err := paths.Prepare(); err != nil {
		return EncodeResult{}, err
	}
	bundle, err := reel.Read(cfg.ReelPath)
	if err != nil {
		return EncodeResult{}, err
	}
	journal, err := task.OpenJournal(paths.Journal)
	if err != nil {
		return EncodeResult{}, err
	}
	defer journal.Close()
	if err := journal.Record("job_started", 0, map[string]any{"resume": resume, "target_bps": cfg.TargetBPS}); err != nil {
		return EncodeResult{}, err
	}
	var plan Plan
	planLoaded := false
	if resume {
		plan, err = loadPlan(paths.Plan)
		if err == nil {
			planLoaded = true
		} else if !os.IsNotExist(err) {
			return EncodeResult{}, err
		}
	}
	if !planLoaded {
		plan, err = buildPlan(ctx, bundle, journal, cfg, paths)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return EncodeResult{Interrupted: true}, fmt.Errorf("%w: %v", ErrInterrupted, err)
			}
			return EncodeResult{}, err
		}
	}
	if err := validatePlan(plan, *bundle, cfg); err != nil {
		return EncodeResult{}, err
	}
	if err := preparePlanStates(journal, plan); err != nil {
		return EncodeResult{}, err
	}
	result := EncodeResult{}
	todo := make([]SegmentPlan, 0, len(plan.Segments))
	for _, sp := range plan.Segments {
		if journal.State(sp.ID) == task.Done {
			if err := validateSegmentFile(paths.Segment(sp.ID), sp, *bundle); err != nil {
				return result, err
			}
			result.Skipped++
			continue
		}
		todo = append(todo, sp)
	}
	if len(todo) > 0 {
		reencoded, interrupted, err := encodeSegments(ctx, paths, journal, cfg, plan, bundle, todo)
		result.Reencoded = reencoded
		result.Interrupted = interrupted
		if err != nil {
			return result, err
		}
	}
	rep, err := buildReportFromPlan(paths, plan)
	if err != nil {
		return result, err
	}
	if err := report.Write(paths.Report, rep); err != nil {
		return result, err
	}
	_ = journal.Record("report_saved", 0, map[string]any{"actual_bps": rep.ActualBPS, "deviation_pct": rep.DeviationPct})
	result.Report = rep
	if math.Abs(rep.DeviationPct) > 2.0 {
		return result, fmt.Errorf("%w: target=%d actual=%.0f deviation=%.3f%%", ErrRateDeviation, rep.TargetBPS, rep.ActualBPS, rep.DeviationPct)
	}
	return result, nil
}

func buildPlan(ctx context.Context, bundle *reel.Bundle, journal *task.Journal, cfg Config, paths Paths) (Plan, error) {
	segCount := len(bundle.Segments)
	for id := 1; id <= segCount; id++ {
		state := journal.State(id)
		if state == task.Done {
			return Plan{}, fmt.Errorf("segment %d is DONE but no allocation plan exists", id)
		}
		if state != task.Pending {
			if err := journal.Transition(id, task.Pending, nil); err != nil {
				return Plan{}, err
			}
		}
	}
	probes := make([][]ProbeSample, segCount)
	qps := probeQPs(cfg.MinQP, cfg.MaxQP)
	tasks := make([]task.Task, segCount)
	var hookMu sync.Mutex
	var hookErr error
	setHookErr := func(err error) {
		if err == nil {
			return
		}
		hookMu.Lock()
		if hookErr == nil {
			hookErr = err
		}
		hookMu.Unlock()
	}
	for i := 0; i < segCount; i++ {
		id := i + 1
		index := i
		tasks[i] = task.Task{ID: id, Run: func(ctx context.Context, attempt int) error {
			from := journal.State(id)
			if from != task.Probing {
				if err := journal.Transition(id, task.Probing, map[string]any{"attempt": attempt}); err != nil {
					return err
				}
			}
			samples := make([]ProbeSample, 0, len(qps))
			for _, qp := range qps {
				if err := ctx.Err(); err != nil {
					return err
				}
				data, err := codec.EncodeSegment(codec.SegmentMeta{ID: id, QP: qp, Width: bundle.Width, Height: bundle.Height, FPS: bundle.FPS, Frames: bundle.FramesPerSeg}, bundle.Segments[index])
				if err != nil {
					return err
				}
				samples = append(samples, ProbeSample{QP: qp, Bits: data.Bits, PSNR: data.PSNR})
			}
			probes[index] = samples
			return journal.Transition(id, task.Planning, nil)
		}}
	}
	sched := task.Scheduler{
		Concurrency: cfg.Concurrency,
		MaxRetries:  cfg.RetryMax,
		BackoffBase: 10 * time.Millisecond,
		MaxBackoff:  400 * time.Millisecond,
		Hooks: task.SchedulerHooks{
			OnRetry: func(id, attempt int, err error) {
				setHookErr(journal.Transition(id, task.Retrying, map[string]any{"attempt": attempt, "error": err.Error()}))
			},
			OnFailure: func(id int, err error) {
				setHookErr(journal.Transition(id, task.Failed, map[string]any{"error": err.Error()}))
			},
		},
	}
	if err := sched.Run(ctx, tasks); err != nil {
		return Plan{}, err
	}
	if hookErr != nil {
		return Plan{}, hookErr
	}
	models := make([]rate.SegmentModel, segCount)
	for i, samples := range probes {
		sq := make([]int, len(samples))
		bits := make([]uint64, len(samples))
		psnrs := make([]float64, len(samples))
		for n, sample := range samples {
			sq[n], bits[n], psnrs[n] = sample.QP, sample.Bits, sample.PSNR
		}
		models[i] = rate.SegmentModel{ID: i + 1, Bits: rate.FitLogBits(sq, bits), Quality: rate.FitQuality(sq, psnrs)}
	}
	duration := float64(bundle.TotalFrames()) / float64(bundle.FPS)
	targetBits := uint64(math.Round(float64(cfg.TargetBPS) * duration))
	allocs, err := rate.Allocate(rate.Request{TargetBits: targetBits, DurationSec: duration, MinQP: cfg.MinQP, MaxQP: cfg.MaxQP, Segments: models})
	if err != nil {
		markPlanningFailed(journal, segCount, err)
		return Plan{}, err
	}
	plan := Plan{
		Version:       planVersion,
		ReelSeed:      bundle.Seed,
		TargetBPS:     cfg.TargetBPS,
		TargetBits:    targetBits,
		DurationSec:   duration,
		FPS:           bundle.FPS,
		SegmentFrames: bundle.FramesPerSeg,
		TotalFrames:   bundle.TotalFrames(),
		MinQP:         cfg.MinQP,
		MaxQP:         cfg.MaxQP,
		Segments:      make([]SegmentPlan, 0, segCount),
	}
	for _, alloc := range allocs {
		plan.Segments = append(plan.Segments, SegmentPlan{ID: alloc.ID, QP: alloc.QP, PredictedBits: alloc.PredictedBits, PredictedPSNR: alloc.PredictedPSNR, Probes: probes[alloc.ID-1]})
	}
	sort.Slice(plan.Segments, func(i, j int) bool { return plan.Segments[i].ID < plan.Segments[j].ID })
	plan, err = calibratePlan(ctx, plan, bundle, cfg)
	if err != nil {
		markPlanningFailed(journal, segCount, err)
		return Plan{}, err
	}
	if err := writeJSONAtomic(paths.Plan, plan); err != nil {
		markPlanningFailed(journal, segCount, err)
		return Plan{}, err
	}
	if err := journal.Record("plan_saved", 0, map[string]any{"target_bits": plan.TargetBits, "segments": len(plan.Segments)}); err != nil {
		return Plan{}, err
	}
	return plan, nil
}
