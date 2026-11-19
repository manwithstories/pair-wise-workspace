// Command avpipe is the terminal entry point for the A/V diagnostic
// pipeline. It exposes two subcommands:
//
//	avpipe selftest [flags]   acceptance run plus isolated fault scenarios
//	avpipe run      [flags]   one full processing run
//
// Exit status: 0 all metrics passed, 2 parameter or input error, 3 metric
// threshold missed, 4 internal processing failure.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"avpipe/internal/pipeline"
	"avpipe/internal/report"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	switch args[0] {
	case "run":
		return runCommand(ctx, args[1:], stdout, stderr, false)
	case "selftest":
		return runCommand(ctx, args[1:], stdout, stderr, true)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "avpipe: unknown subcommand %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `avpipe - local A/V pipeline diagnostics

usage:
  avpipe run      [flags]
  avpipe selftest [flags]

flags:
  --seed               uint     deterministic seed (default 1)
  --input              string   input source; only "generated" is supported
  --sample-rate        int      target sample rate (default 48000)
  --source-sample-rate int      generated source rate (default 48000)
  --layout             string   target channel layout (default stereo)
  --source-layout      string   generated source layout (default 5.1)
  --target-lufs        float    loudness target (default -16)
  --peak-ceiling       float    peak ceiling 0..1 (default 0.95)
  --loss               float    simulated RTP loss 0..1 (default 0.08)
  --jitter-ms          float    simulated jitter (default 15)
  --consumer-delay-ms  float    per-block consumer service time (default 0)
  --drift-ppm          float    simulated A/V clock drift (default 600)
  --recovery-window    int      RTP recovery window in packets (default 16)
  --queue-bytes        int      queue byte ceiling (default 262144)
  --queue-ms           float    queue duration ceiling (default 250)
  --policy             string   block | drop | drop-oldest (default block)
  --frames             int      generated frames (default one second)
  --block-size         int      PCM block frames (default 1024)
  --report             string   write JSON report to this path
  --work-dir           string   scratch directory for the MP4 clip

exit status: 0 pass, 2 parameter/input error, 3 metric threshold missed,
4 internal processing failure.
`)
}

func runCommand(ctx context.Context, args []string, stdout, stderr io.Writer, self bool) int {
	opts := pipeline.Options{Loss: 0.08, JitterMs: 15}
	var (
		layout     = "stereo"
		srcLayout  = "5.1"
		input      = "generated"
		reportPath string
		queueMs    = 250.0
	)
	fs := flag.NewFlagSet("avpipe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Uint64Var(&opts.Seed, "seed", 1, "deterministic seed")
	fs.StringVar(&input, "input", "generated", "input source (generated)")
	fs.IntVar(&opts.TargetRate, "sample-rate", 48000, "target sample rate")
	fs.IntVar(&opts.SourceRate, "source-sample-rate", 48000, "generated source rate")
	fs.StringVar(&layout, "layout", "stereo", "target channel layout")
	fs.StringVar(&srcLayout, "source-layout", "5.1", "generated source layout")
	fs.Float64Var(&opts.TargetLUFS, "target-lufs", -16, "loudness target")
	fs.Float64Var(&opts.PeakCeiling, "peak-ceiling", 0.95, "peak ceiling")
	fs.Float64Var(&opts.Loss, "loss", 0.08, "simulated RTP loss")
	fs.Float64Var(&opts.JitterMs, "jitter-ms", 15, "simulated jitter in ms")
	fs.Float64Var(&opts.ConsumerDelayMs, "consumer-delay-ms", 0, "consumer service time per block")
	fs.Float64Var(&opts.DriftPPM, "drift-ppm", 600, "simulated clock drift")
	fs.IntVar(&opts.RecoveryWindow, "recovery-window", 16, "RTP recovery window")
	fs.Int64Var(&opts.QueueBytes, "queue-bytes", 262144, "queue byte ceiling")
	fs.Float64Var(&queueMs, "queue-ms", 250, "queue duration ceiling in ms")
	fs.StringVar(&opts.Policy, "policy", "block", "block|drop|drop-oldest")
	fs.IntVar(&opts.Frames, "frames", 0, "generated frames (0 = one second)")
	fs.IntVar(&opts.BlockSize, "block-size", 1024, "PCM block frames")
	fs.StringVar(&reportPath, "report", "", "write JSON report to path")
	fs.StringVar(&opts.WorkDir, "work-dir", "", "scratch directory for the MP4 clip")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "avpipe: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if input != "generated" {
		fmt.Fprintf(stderr, "avpipe: --input %q is not supported; only \"generated\" is available in this local build\n", input)
		return 2
	}
	if opts.Policy != "block" && opts.Policy != "drop" && opts.Policy != "drop-oldest" {
		fmt.Fprintf(stderr, "avpipe: --policy %q must be block, drop or drop-oldest\n", opts.Policy)
		return 2
	}
	if err := pipeline.ValidateOptions(opts, layout, srcLayout, queueMs); err != nil {
		fmt.Fprintf(stderr, "avpipe: %v\n", err)
		return 2
	}
	opts.Layout, opts.SourceLayout = layout, srcLayout
	opts.Input = input
	opts.QueueTicks = int64(queueMs * 90)
	opts.ReportPath = reportPath

	var rep *report.Report
	if self {
		rep = pipeline.SelfTest(ctx, opts)
	} else {
		rep = pipeline.Run(ctx, opts)
	}
	printSummary(stdout, rep, self)
	if reportPath != "" {
		if err := rep.WriteEncode(reportPath); err != nil {
			fmt.Fprintf(stderr, "avpipe: %v\n", err)
			return 4
		}
		fmt.Fprintf(stdout, "report written to %s\n", reportPath)
	}
	if rep.ExitCode == 0 && !rep.Passed {
		return 3
	}
	return rep.ExitCode
}
