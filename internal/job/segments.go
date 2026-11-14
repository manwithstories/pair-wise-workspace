package job

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"rcx/internal/codec"
	"rcx/internal/quality"
	"rcx/internal/reel"
	"rcx/internal/report"
	"rcx/internal/task"
)

func markPlanningFailed(journal *task.Journal, segCount int, cause error) {
	for id := 1; id <= segCount; id++ {
		_ = journal.Transition(id, task.Failed, map[string]any{"error": cause.Error()})
	}
}

func probeQPs(minQP, maxQP int) []int {
	candidates := []int{minQP, minQP + (maxQP-minQP)/3, minQP + 2*(maxQP-minQP)/3, maxQP}
	seen := make(map[int]bool)
	out := make([]int, 0, len(candidates))
	for _, q := range candidates {
		if q < minQP {
			q = minQP
		}
		if q > maxQP {
			q = maxQP
		}
		if !seen[q] {
			seen[q] = true
			out = append(out, q)
		}
	}
	sort.Ints(out)
	return out
}

func validatePlan(plan Plan, bundle reel.Bundle, cfg Config) error {
	if len(plan.Segments) != len(bundle.Segments) || plan.TargetBPS != cfg.TargetBPS || plan.FPS != bundle.FPS || plan.SegmentFrames != bundle.FramesPerSeg {
		return errors.New("saved allocation plan does not match job/bundle")
	}
	for i, sp := range plan.Segments {
		if sp.ID != i+1 || sp.QP < cfg.MinQP || sp.QP > cfg.MaxQP {
			return errors.New("saved allocation plan has invalid segment")
		}
	}
	return nil
}

func preparePlanStates(journal *task.Journal, plan Plan) error {
	for _, sp := range plan.Segments {
		state := journal.State(sp.ID)
		if state == task.Done {
			continue
		}
		if state != task.Pending {
			if err := journal.Transition(sp.ID, task.Pending, nil); err != nil {
				return err
			}
		}
		if err := journal.Transition(sp.ID, task.Planning, map[string]any{"qp": sp.QP}); err != nil {
			return err
		}
	}
	return nil
}

func encodeSegments(ctx context.Context, paths Paths, journal *task.Journal, cfg Config, plan Plan, bundle *reel.Bundle, todo []SegmentPlan) (int, bool, error) {
	concurrency := cfg.Concurrency
	if cfg.CrashAfterSegment > 0 {
		concurrency = 1
	}
	var countMu sync.Mutex
	reencoded := 0
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
	tasks := make([]task.Task, len(todo))
	for i, sp := range todo {
		sp := sp
		tasks[i] = task.Task{ID: sp.ID, Run: func(ctx context.Context, attempt int) error {
			current := journal.State(sp.ID)
			if current == task.Retrying {
				if err := journal.Transition(sp.ID, task.Encoding, map[string]any{"attempt": attempt}); err != nil {
					return err
				}
			} else if current == task.Planning {
				if err := journal.Transition(sp.ID, task.Encoding, map[string]any{"attempt": attempt}); err != nil {
					return err
				}
			} else {
				if err := journal.Transition(sp.ID, task.Planning, nil); err != nil {
					return err
				}
				if err := journal.Transition(sp.ID, task.Encoding, map[string]any{"attempt": attempt}); err != nil {
					return err
				}
			}
			meta := codec.SegmentMeta{ID: sp.ID, QP: sp.QP, Width: bundle.Width, Height: bundle.Height, FPS: bundle.FPS, Frames: bundle.FramesPerSeg}
			data, err := codec.EncodeSegment(meta, bundle.Segments[sp.ID-1])
			if err != nil {
				return err
			}
			path := paths.Segment(sp.ID)
			if err := codec.WriteAtomic(path, data.Bytes); err != nil {
				return err
			}
			if err := journal.Transition(sp.ID, task.Verifying, nil); err != nil {
				return err
			}
			if err := validateSegmentFile(path, sp, *bundle); err != nil {
				return err
			}
			if err := journal.Transition(sp.ID, task.Done, map[string]any{"qp": sp.QP, "bits": data.Bits, "crc": data.CRC, "psnr_db": data.PSNR}); err != nil {
				return err
			}
			countMu.Lock()
			reencoded++
			countMu.Unlock()
			return nil
		}}
	}
	sched := task.Scheduler{
		Concurrency:  concurrency,
		MaxRetries:   cfg.RetryMax,
		BackoffBase:  20 * time.Millisecond,
		MaxBackoff:   1 * time.Second,
		SuccessLimit: cfg.CrashAfterSegment,
		Hooks: task.SchedulerHooks{
			OnRetry: func(id, attempt int, err error) {
				setHookErr(journal.Transition(id, task.Retrying, map[string]any{"attempt": attempt, "error": err.Error()}))
			},
			OnFailure: func(id int, err error) {
				setHookErr(journal.Transition(id, task.Failed, map[string]any{"error": err.Error()}))
			},
		},
	}
	err := sched.Run(ctx, tasks)
	countMu.Lock()
	done := reencoded
	countMu.Unlock()
	if errors.Is(err, task.ErrSuccessLimit) {
		return done, true, fmt.Errorf("%w: completed_segments=%d", ErrInterrupted, done)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return done, true, fmt.Errorf("%w: %v", ErrInterrupted, err)
		}
		return done, false, err
	}
	if hookErr != nil {
		return done, false, hookErr
	}
	return done, false, nil
}

func validateSegmentFile(path string, sp SegmentPlan, bundle reel.Bundle) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: read segment %d: %v", codec.ErrCorrupt, sp.ID, err)
	}
	decoded, err := codec.DecodeSegment(data)
	if err != nil {
		return err
	}
	if decoded.Meta.ID != sp.ID || decoded.Meta.QP != sp.QP || decoded.Meta.Width != bundle.Width || decoded.Meta.Height != bundle.Height || decoded.Meta.Frames != bundle.FramesPerSeg || decoded.Meta.FPS != bundle.FPS {
		return fmt.Errorf("%w: segment %d metadata differs from plan", codec.ErrCorrupt, sp.ID)
	}
	measured := quality.SegmentPSNR(bundle.Segments[sp.ID-1], decoded.Frames)
	if math.Abs(measured-decoded.PSNR) > 1e-6 {
		return fmt.Errorf("%w: segment %d stored PSNR mismatch", codec.ErrCorrupt, sp.ID)
	}
	return nil
}

func buildReportFromPlan(paths Paths, plan Plan) (report.Report, error) {
	segments := make([]report.SegmentReport, 0, len(plan.Segments))
	for _, sp := range plan.Segments {
		data, err := os.ReadFile(paths.Segment(sp.ID))
		if err != nil {
			return report.Report{}, fmt.Errorf("%w: read segment %d: %v", codec.ErrCorrupt, sp.ID, err)
		}
		decoded, err := codec.DecodeSegment(data)
		if err != nil {
			return report.Report{}, err
		}
		if decoded.Meta.ID != sp.ID || decoded.Meta.QP != sp.QP || decoded.Meta.FPS != plan.FPS || decoded.Meta.Frames != plan.SegmentFrames {
			return report.Report{}, fmt.Errorf("%w: segment %d metadata differs from plan", codec.ErrCorrupt, sp.ID)
		}
		segments = append(segments, report.SegmentReport{
			ID:     decoded.Meta.ID,
			Bits:   decoded.Bits,
			BPS:    float64(decoded.Bits) * float64(decoded.Meta.FPS) / float64(decoded.Meta.Frames),
			QP:     decoded.Meta.QP,
			PSNRDB: decoded.PSNR,
			CRC:    decoded.CRC,
		})
	}
	return report.Build(plan.TargetBPS, plan.TargetBits, plan.DurationSec, segments), nil
}
