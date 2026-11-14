package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"rcx/internal/codec"
	"rcx/internal/job"
	"rcx/internal/rate"
	"rcx/internal/reel"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return 1
	}
	switch args[0] {
	case "reel":
		return runReel(args[1:])
	case "encode":
		return runEncode(args[1:])
	case "resume":
		return runResume(args[1:])
	case "report":
		return runReport(args[1:])
	case "regress":
		return runRegress(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		printUsage()
		return 1
	}
}

func runReel(args []string) int {
	if len(args) == 0 || args[0] != "gen" {
		fmt.Fprintln(os.Stderr, "usage: rcx reel gen --seed N --segments N --out PATH")
		return 1
	}
	fs := flag.NewFlagSet("reel gen", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	seed := fs.Int64("seed", 7, "deterministic scene seed")
	segments := fs.Int("segments", 12, "number of segments")
	out := fs.String("out", "", "output reel path")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "--out is required")
		return 1
	}
	bundle, err := reel.Generate(*seed, *segments)
	if err != nil {
		return reportError(err)
	}
	if err := reel.Write(*out, bundle); err != nil {
		return reportError(err)
	}
	fmt.Printf("reel=%s segments=%d frames_per_segment=%d width=%d height=%d fps=%d seed=%d\n", *out, len(bundle.Segments), bundle.FramesPerSeg, bundle.Width, bundle.Height, bundle.FPS, *seed)
	return 0
}

func runEncode(args []string) int {
	fs := flag.NewFlagSet("encode", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	reelPath := fs.String("reel", "", "source reel path")
	targetBPS := fs.Int64("target-bps", 1_500_000, "total video bitrate budget")
	minQP := fs.Int("min-qp", 18, "minimum QP")
	maxQP := fs.Int("max-qp", 44, "maximum QP")
	concurrency := fs.Int("concurrency", 4, "segment encoding workers")
	retryMax := fs.Int("retry-max", 2, "per-segment retry limit")
	crashAfter := fs.Int("crash-after-segment", 0, "simulate interruption after N completed segments")
	jobDir := fs.String("job", "", "job directory")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *reelPath == "" || *jobDir == "" {
		fmt.Fprintln(os.Stderr, "--reel and --job are required")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := job.RunEncode(ctx, job.EncodeOptions{
		ReelPath: *reelPath, TargetBPS: *targetBPS, MinQP: *minQP, MaxQP: *maxQP,
		Concurrency: *concurrency, RetryMax: *retryMax, CrashAfterSegment: *crashAfter, JobDir: *jobDir,
	})
	if err != nil {
		if errors.Is(err, job.ErrInterrupted) {
			fmt.Printf("status=INTERRUPTED completed_segments=%d job=%s\n", result.Reencoded, *jobDir)
		}
		return reportError(err)
	}
	fmt.Printf("target_bps=%d actual_bps=%.0f deviation_pct=%.3f psnr_mean_db=%.3f psnr_segment_std_db=%.3f segments=%d job=%s\n",
		result.Report.TargetBPS, result.Report.ActualBPS, result.Report.DeviationPct, result.Report.PSNRMeanDB, result.Report.PSNRSegmentStdDB, result.Report.SegmentCount, *jobDir)
	return 0
}

func runResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jobDir := fs.String("job", "", "job directory")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *jobDir == "" {
		fmt.Fprintln(os.Stderr, "--job is required")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := job.RunResume(ctx, *jobDir)
	if err != nil {
		return reportError(err)
	}
	fmt.Printf("skipped_segments=%d reencoded_segments=%d actual_bps=%.0f deviation_pct=%.3f job=%s\n", result.Skipped, result.Reencoded, result.Report.ActualBPS, result.Report.DeviationPct, *jobDir)
	return 0
}

func runReport(args []string) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jobDir := fs.String("job", "", "job directory")
	jsonOut := fs.String("json-out", "", "optional JSON output inside job directory")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *jobDir == "" {
		fmt.Fprintln(os.Stderr, "--job is required")
		return 1
	}
	rep, err := job.RunReport(*jobDir, *jsonOut)
	if err != nil {
		return reportError(err)
	}
	if err := printJSON(rep); err != nil {
		return reportError(err)
	}
	return 0
}

func runRegress(args []string) int {
	fs := flag.NewFlagSet("regress", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	baseline := fs.String("baseline", "", "baseline job directory")
	candidate := fs.String("candidate", "", "candidate job directory")
	threshold := fs.Float64("drop-threshold", 0.8, "PSNR drop threshold in dB")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *baseline == "" || *candidate == "" {
		fmt.Fprintln(os.Stderr, "--baseline and --candidate are required")
		return 1
	}
	result, err := job.RunRegress(*baseline, *candidate, *threshold)
	if err != nil && !errors.Is(err, job.ErrRegression) {
		return reportError(err)
	}
	if err := printJSON(result); err != nil {
		return reportError(err)
	}
	if errors.Is(err, job.ErrRegression) {
		return 3
	}
	return 0
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jobDir := fs.String("job", "", "job directory")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *jobDir == "" {
		fmt.Fprintln(os.Stderr, "--job is required")
		return 1
	}
	count, err := job.RunVerify(*jobDir)
	if err != nil {
		return reportError(err)
	}
	fmt.Printf("verified_segments=%d status=OK job=%s\n", count, *jobDir)
	return 0
}

func printJSON(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func reportError(err error) int {
	var infeasible *rate.InfeasibleError
	if errors.As(err, &infeasible) {
		fmt.Fprintf(os.Stderr, "error: %v; target_bits=%d minimum_bits=%d minimum_bps=%.0f maximum_bps=%.0f\n",
			err, infeasible.TargetBits, infeasible.MinimumBits, infeasible.MinimumBPS, infeasible.MaximumBPS)
	} else {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	switch {
	case errors.Is(err, rate.ErrInfeasible), errors.Is(err, job.ErrRateDeviation):
		return 2
	case errors.Is(err, job.ErrRegression):
		return 3
	case errors.Is(err, reel.ErrCorrupt), errors.Is(err, codec.ErrCorrupt):
		return 4
	case errors.Is(err, job.ErrInterrupted):
		return 5
	default:
		return 1
	}
}

func printUsage() {
	fmt.Print(`rcx - deterministic terminal reel transcoder

Usage:
  rcx reel gen --seed N --segments N --out PATH
  rcx encode --reel PATH --target-bps N --min-qp N --max-qp N --concurrency N --job DIR
  rcx resume --job DIR
  rcx verify --job DIR
  rcx report --job DIR [--json-out PATH]
  rcx regress --baseline DIR --candidate DIR --drop-threshold DB

Exit codes: 0 success, 1 internal, 2 infeasible/deviation, 3 regression,
4 corrupt input/bitstream, 5 interrupted and resumable.
`)
}
