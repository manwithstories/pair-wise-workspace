// Command mediacq runs the deterministic broadcast media quality pipeline.
// It generates synthetic success/corruption/recovery fixtures, streams them
// through the bounded pipeline, and prints key=value metrics plus built-in
// assertion results. Exit codes: 0 all assertions pass, 1 input/quality
// assertion failure, 2 parameter error.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"mediacq/internal/app"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: mediacq demo [flags]")
		os.Exit(2)
	}

	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	cfg := app.DefaultConfig()
	fs.Int64Var(&cfg.Seed, "seed", cfg.Seed, "deterministic fixture seed")
	fs.Float64Var(&cfg.DriftPPM, "drift-ppm", 0, "sender clock drift in ppm")
	wrapAt := fs.Uint64("wrap-at", 0, "RTP timestamp wrap boundary (0 disables)")
	fs.BoolVar(&cfg.VFR, "vfr", false, "emit variable frame rate video")
	fs.IntVar(&cfg.QueueCapacity, "queue", cfg.QueueCapacity, "bounded queue capacity")
	fs.Int64Var(&cfg.TargetBPS, "target-bitrate", cfg.TargetBPS, "target bitrate in bps")
	fs.BoolVar(&cfg.CorruptBox, "corrupt-box", false, "generate a corrupt box")
	fs.BoolVar(&cfg.Truncate, "truncate", false, "truncate the mdat")
	fs.IntVar(&cfg.CancelAfter, "cancel-after", 0, "cancel after N commits")
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	if cfg.QueueCapacity < 1 {
		fmt.Fprintln(os.Stderr, "queue capacity must be >= 1")
		os.Exit(2)
	}
	cfg.WrapAt = uint32(*wrapAt)
	cfg.TargetSegment = 500 * time.Millisecond

	os.Exit(run(cfg))
}

func run(cfg app.Config) int {
	ctx := context.Background()
	res, err := app.Run(ctx, cfg)

	// Recovery run is meaningful for the success path and for cancel demos.
	var resumeMatch bool
	var resumeSegments int
	if cfg.CancelAfter > 0 && err == nil {
		_, resumed, match, rerr := app.RunResume(cfg)
		if rerr == nil && resumed != nil {
			resumeMatch = match
			resumeSegments = len(resumed.Manifest.Segments)
		}
	}

	if res != nil {
		printMetrics(res, cfg, resumeSegments, resumeMatch)
	}

	// Failure-mode invocation: a corrupted/truncated input must surface a
	// quality error and the cancel demo must retain exactly the committed set.
	if cfg.CorruptBox || cfg.Truncate || cfg.MissingKey {
		if err != nil || res == nil || len(res.Metrics.Errors) == 0 {
			fmt.Printf("assertion=FAILED expected quality error from corrupt input (err=%v)\n", err)
			return 1
		}
		if cfg.CancelAfter > 0 && res.Metrics.Segments != cfg.CancelAfter {
			fmt.Printf("assertion=FAILED committed_segments=%d want %d\n",
				res.Metrics.Segments, cfg.CancelAfter)
			return 1
		}
		fmt.Println("assertion=PASSED")
		return 0
	}

	if err != nil {
		fmt.Printf("assertion=FAILED input error: %v\n", err)
		return 1
	}

	// Success-path built-in assertions.
	failed := false
	if !res.Metrics.AssertBitrate(2.0) {
		fmt.Printf("assertion=FAILED bitrate_delta_pct=%.3f exceeds 2%%\n", res.Metrics.BitrateDeltaPct)
		failed = true
	}
	if !res.Metrics.AssertAlignment(20) {
		fmt.Printf("assertion=FAILED manifest_alignment_ms=%d exceeds 20\n", res.Metrics.AlignmentMS)
		failed = true
	}
	if res.Metrics.PeakQueue > cfg.QueueCapacity {
		fmt.Printf("assertion=FAILED peak_queue=%d exceeds capacity %d\n",
			res.Metrics.PeakQueue, cfg.QueueCapacity)
		failed = true
	}
	if cfg.CancelAfter > 0 {
		if res.Metrics.Segments != cfg.CancelAfter {
			fmt.Printf("assertion=FAILED committed_segments=%d want %d\n",
				res.Metrics.Segments, cfg.CancelAfter)
			failed = true
		}
		if !resumeMatch {
			fmt.Println("assertion=FAILED resume fingerprints do not match continuous run")
			failed = true
		}
	}
	if failed {
		return 1
	}
	fmt.Println("assertion=PASSED")
	return 0
}

func printMetrics(res *app.Result, cfg app.Config, resumeSegments int, resumeMatch bool) {
	m := res.Metrics
	fmt.Printf("segments=%d silence_ms=%d duplicates=%d target_bps=%d actual_bps=%d bitrate_delta_pct=%.2f quality_errors=%d manifest_alignment_ms=%d peak_queue=%d\n",
		m.Segments, m.SilenceMS, m.Duplicates, m.TargetBPS, m.ActualBPS,
		m.BitrateDeltaPct, len(m.Errors), m.AlignmentMS, m.PeakQueue)
	fmt.Printf("drift_windows=%d canceled=%v\n", m.DriftWindows, res.Canceled)
	if cfg.CancelAfter > 0 {
		fmt.Printf("committed_segments=%d resume_segments=%d resume_match=%v manifest_alignment_ms=%d\n",
			m.Segments, resumeSegments, resumeMatch, m.AlignmentMS)
	}
}
