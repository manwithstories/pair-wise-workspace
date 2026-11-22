// Command mediad drives the acceptance demo and independent verification.
// It is a thin shell over the library packages: all media logic lives under
// internal/.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediad/internal/fixture"
	"mediad/internal/worker"
)

// Exit codes are part of the acceptance contract.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: mediad <demo|verify> [flags]")
		return exitUsage
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "mediad: unknown command %q\n", args[0])
		printUsage(stderr)
		return exitUsage
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `usage:
  mediad demo   --seed 42 --chunk-size 8 --workers 4 --resume [--dir DIR] [--segments N]
  mediad verify --input DIR

Exit codes: 0 all thresholds passed, 1 acceptance metric failed, 2 usage or input error.`)
}

func runDemo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Int64("seed", 42, "fixture seed")
	chunkSize := fs.Int("chunk-size", 8, "segments per checkpoint chunk")
	workers := fs.Int("workers", 4, "concurrent workers")
	resume := fs.Bool("resume", false, "resume from an existing checkpoint")
	dir := fs.String("dir", "", "batch directory (default: temp dir)")
	segments := fs.Int("segments", 24, "segments per case")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "mediad: unexpected positional arguments")
		return exitUsage
	}
	opts := worker.DefaultOptions()
	opts.ChunkSize, opts.Workers = *chunkSize, *workers
	if err := opts.Validate(); err != nil {
		fmt.Fprintf(stderr, "mediad: %v\n", err)
		return exitUsage
	}
	if *segments <= 0 || *segments > 10000 {
		fmt.Fprintf(stderr, "mediad: segment count %d out of range\n", *segments)
		return exitUsage
	}
	batchDir := *dir
	if batchDir == "" {
		tmp, err := os.MkdirTemp("", "mediad-demo-*")
		if err != nil {
			fmt.Fprintf(stderr, "mediad: %v\n", err)
			return exitFailure
		}
		batchDir = tmp
	}
	absDir, err := filepath.Abs(batchDir)
	if err != nil {
		fmt.Fprintf(stderr, "mediad: %v\n", err)
		return exitUsage
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "mediad: %v\n", err)
		return exitFailure
	}

	fixtureOpts := fixture.DefaultOptions(absDir, *seed)
	fixtureOpts.Segments = *segments
	batches, err := fixture.Generate(fixtureOpts)
	if err != nil {
		fmt.Fprintf(stderr, "mediad: generate fixtures: %v\n", err)
		return exitFailure
	}
	reports := make([]caseReport, 0, len(batches))
	for _, b := range batches {
		rep, err := runCase(context.Background(), b, opts, absDir, *resume)
		if err != nil {
			fmt.Fprintf(stderr, "mediad: case %s: %v\n", b.Case, err)
			return exitFailure
		}
		reports = append(reports, rep)
	}
	printDemo(stdout, absDir, reports)
	if err := writeReport(absDir, reports); err != nil {
		fmt.Fprintf(stderr, "mediad: write report: %v\n", err)
		return exitFailure
	}
	if !thresholdsPass(reports) {
		return exitFailure
	}
	return exitOK
}

func printDemo(w io.Writer, dir string, reports []caseReport) {
	fmt.Fprintf(w, "batch_dir=%s\n", dir)
	for _, r := range reports {
		fmt.Fprintf(w, "case=%s manifest_roundtrip=%.1f%% keyframe_hit=%.1f%% segments_ok=%d segments_bad=%d recovered_chunks=%d sha256_match=%d/%d timeline_gaps=%d\n",
			r.Case, r.RoundTripPct, r.KeyframeHitPct, r.SegmentsOK, r.SegmentsBad,
			r.RecoveredChunks, r.HashMatched, r.HashTotal, r.TimelineGaps+r.TimelineJumps)
		if !r.RTPMonotonic {
			fmt.Fprintf(w, "  rtp_warning case=%s evidence timestamps are not monotonic\n", r.Case)
		}
	}
	for _, r := range reports {
		if r.Case == string(fixture.CaseNormal) {
			fmt.Fprintf(w, "summary manifest_roundtrip=%.1f%% keyframe_hit=%.1f%% segments_ok=%d segments_bad=%d recovered_chunks=%d sha256_match=%d/%d timeline_gaps=%d\n",
				r.RoundTripPct, r.KeyframeHitPct, r.SegmentsOK, r.SegmentsBad,
				r.RecoveredChunks, r.HashMatched, r.HashTotal, r.TimelineGaps+r.TimelineJumps)
			if !r.RTPMonotonic {
				fmt.Fprintf(w, "  rtp_warning case=%s evidence timestamps are not monotonic\n", r.Case)
			}
		}
	}
}
