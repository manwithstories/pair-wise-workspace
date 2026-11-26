package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mediascan/internal/parser"
	"mediascan/internal/pipeline"
	"mediascan/internal/report"
	"mediascan/internal/samplegen"
	"mediascan/internal/store"
)

type demoOptions struct {
	WorkDir string
	Workers int
	Seed    int64
}

func runDemo(ctx context.Context, opts demoOptions) (string, int, error) {
	absWork, err := filepath.Abs(filepath.Clean(opts.WorkDir))
	if err != nil {
		return "", 1, err
	}
	// Every demo run recreates the deterministic interrupted state so the
	// printed resume_skipped count is stable.
	if err := os.RemoveAll(absWork); err != nil {
		return "", 1, err
	}
	if err := os.MkdirAll(absWork, 0o755); err != nil {
		return "", 1, err
	}

	set, err := samplegen.Generate(absWork, opts.Seed)
	if err != nil {
		return "", 1, fmt.Errorf("generate fixtures: %w", err)
	}
	st, err := store.New(filepath.Join(absWork, "checkpoints"))
	if err != nil {
		return "", 1, err
	}
	params := store.Params{Workers: opts.Workers, Seed: opts.Seed}.Normalize()
	jobs, err := demoJobs(set, params)
	if err != nil {
		return "", 1, err
	}

	// Deterministically interrupt a one-worker first pass immediately after
	// the third durable commit. This proves cancellation and atomic resume.
	interruptCtx, cancelInterrupt := context.WithCancel(ctx)
	committed := 0
	first := pipeline.Run(interruptCtx, jobs, pipeline.Options{
		Workers: 1,
		Store:   st,
		OnCommitted: func(string) {
			committed++
			if committed == 3 {
				cancelInterrupt()
			}
		},
	})
	cancelInterrupt()
	if len(first) != 3 {
		return "", 1, fmt.Errorf("interrupted pass committed %d outcomes, want 3", len(first))
	}

	start := time.Now()
	outcomes := pipeline.Run(ctx, jobs, pipeline.Options{Workers: params.Workers, Store: st})
	resumeMS := time.Since(start).Milliseconds()
	for _, o := range outcomes {
		if o.FingerprintMismatch {
			return "", 4, o.Err
		}
		if o.Err != nil {
			return "", 1, o.Err
		}
	}

	rep, err := pipeline.OrderedMerge(ctx, outcomes, pipeline.MergeConfig{
		Parameters:    params,
		CheckpointRel: "checkpoints",
	})
	if err != nil {
		return "", 1, err
	}
	if err := verifyDemoExpectations(set, rep); err != nil {
		return "", 1, err
	}

	reportPath := filepath.Join(absWork, "report.json")
	hash, err := report.WriteCanonical(reportPath, rep)
	if err != nil {
		return "", 1, err
	}
	if got, err := report.ReadHash(reportPath); err != nil || got != hash {
		return "", 1, fmt.Errorf("report hash verification failed: %w", err)
	}

	fmt.Printf("files=%d\n", rep.Metrics.Files)
	fmt.Printf("tracks=%d\n", rep.Metrics.Tracks)
	fmt.Printf("corrupt_regions=%d\n", rep.Metrics.CorruptRegions)
	fmt.Printf("offset_error_max_bytes=%d\n", rep.Metrics.OffsetErrorMaxBytes)
	fmt.Printf("keyframe_time_error_max_ticks=%d\n", rep.Metrics.KeyframeTimeErrorMaxTicks)
	fmt.Printf("resume_skipped=%d\n", rep.Metrics.ResumeSkipped)
	fmt.Printf("resume_ms=%d\n", resumeMS)
	fmt.Printf("report_sha256=%s\n", hash)
	return hash, 0, nil
}

func demoJobs(set *samplegen.FixtureSet, params store.Params) ([]pipeline.Job, error) {
	jobs := make([]pipeline.Job, 0, len(set.Files))
	for _, f := range set.Files {
		src, err := parser.OpenSource(f.Path)
		if err != nil {
			return nil, err
		}
		hash := src.SHA256
		_ = src.Close()
		abs, err := filepath.Abs(f.Path)
		if err != nil {
			return nil, err
		}
		task, err := store.NewTask(store.JobID(abs), f.Name, hash, params)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, pipeline.Job{Index: f.Index, Path: f.Path, Task: task})
	}
	return jobs, nil
}
