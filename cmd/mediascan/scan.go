package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mediascan/internal/parser"
	"mediascan/internal/pipeline"
	"mediascan/internal/report"
	"mediascan/internal/store"
)

func runScanCommand(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	work := fs.String("work", "./.scan", "checkpoint/report workspace")
	workers := fs.Int("workers", 4, "worker concurrency (1-16)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *workers < 1 || *workers > 16 {
		fmt.Fprintf(os.Stderr, "workers must be between 1 and 16\n")
		return 1
	}
	paths, err := expandInputs(fs.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "input error: %v\n", err)
		return 1
	}
	absWork, err := filepath.Abs(*work)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	st, err := store.New(filepath.Join(absWork, "checkpoints"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	params := store.Params{Workers: *workers, Seed: 0}
	jobs, err := jobsForPaths(paths, params)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	outcomes := pipeline.Run(ctx, jobs, pipeline.Options{Workers: *workers, Store: st})
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "scan interrupted; committed tasks can resume")
		return 3
	}
	for _, o := range outcomes {
		if o.FingerprintMismatch {
			fmt.Fprintf(os.Stderr, "%v\n", o.Err)
			return 4
		}
		if o.Err != nil {
			fmt.Fprintf(os.Stderr, "scan failed: %v\n", o.Err)
			return 1
		}
	}
	rep, err := pipeline.OrderedMerge(ctx, outcomes, pipeline.MergeConfig{Parameters: params, CheckpointRel: "checkpoints"})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	reportPath := filepath.Join(absWork, "report.json")
	hash, err := report.WriteCanonical(reportPath, rep)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("files=%d tracks=%d corrupt_regions=%d resume_skipped=%d\n", rep.Metrics.Files, rep.Metrics.Tracks, rep.Metrics.CorruptRegions, rep.Metrics.ResumeSkipped)
	fmt.Printf("report_sha256=%s\n", hash)
	if rep.Metrics.CorruptRegions > 0 || rep.Metrics.ZeroTimescaleCount > 0 {
		return 2
	}
	return 0
}

func expandInputs(args []string) ([]string, error) {
	if len(args) == 0 {
		args = []string{"."}
	}
	var paths []string
	for _, arg := range args {
		st, err := os.Stat(arg)
		if err != nil {
			return nil, err
		}
		if st.IsDir() {
			found, err := pipeline.Discover(arg)
			if err != nil {
				return nil, err
			}
			paths = append(paths, found...)
		} else {
			paths = append(paths, arg)
		}
	}
	return paths, nil
}

func jobsForPaths(paths []string, params store.Params) ([]pipeline.Job, error) {
	jobs := make([]pipeline.Job, 0, len(paths))
	for i, p := range paths {
		src, err := parser.OpenSource(p)
		if err != nil {
			return nil, err
		}
		hash := src.SHA256
		_ = src.Close()
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		task, err := store.NewTask(store.JobID(abs), filepath.Base(abs), hash, params)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, pipeline.Job{Index: i, Path: abs, Task: task})
	}
	return jobs, nil
}
