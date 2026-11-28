package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"presspack/internal/core"
)

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", "", "demo output directory to audit")
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "verify: --dir is required")
		return exitFailure
	}
	if err := checkDeps(); err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		return exitFailure
	}
	rep, err := readReport(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify: read report:", err)
		return exitFailure
	}
	// Safe rerun: the pipeline resumes completed variants and must not
	// change a single output byte.
	opts := pipeOptions{segLen: core.FromMillis(rep.SegmentMs), targets: rep.Targets}
	ctx := context.Background()
	rep2, err := runPipeline(ctx, filepath.Join(*dir, "src"), filepath.Join(*dir, "delivery"), opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify: pipeline rerun:", err)
		return exitFailure
	}
	rep2.Seed, rep2.Seconds = rep.Seed, rep.Seconds
	repeat := "match"
	if rep.DeliveryHash != rep2.DeliveryHash {
		repeat = "mismatch"
	}
	metrics, err := computeMetrics(ctx, *dir, rep2)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify: metrics:", err)
		return exitFailure
	}
	metrics.RepeatHash = repeat
	if err := writeReport(*dir, rep2); err != nil {
		fmt.Fprintln(os.Stderr, "verify: report:", err)
		return exitFailure
	}
	metrics.Print(os.Stdout)
	if rep2.partial() {
		fmt.Fprintln(os.Stdout, "status=partial_delivery")
		return exitPartial
	}
	if !metrics.Pass() {
		fmt.Fprintln(os.Stdout, "status=metrics_failed")
		return exitPartial
	}
	fmt.Fprintln(os.Stdout, "status=pass")
	return exitOK
}
