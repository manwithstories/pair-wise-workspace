// Command mediapipeline runs the self-contained real-time media pipeline as a
// terminal tool. It generates deterministic RTP/RTCP fixtures, pushes them
// through parse/reorder/audio/container stages under byte and packet bounds,
// and prints a quantified summary. No network, device or GUI is used.
//
// Exit codes:
//
//	0 - all quantitative assertions passed
//	1 - a quantitative assertion failed
//	2 - invalid parameters or input
//	3 - processing was canceled or hit an unrecoverable error
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"mediapipeline/internal/mediaerr"
	"mediapipeline/internal/metrics"
	"mediapipeline/internal/pipeline"
	"mediapipeline/internal/testdata"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches subcommands and maps failures onto the documented exit codes.
func run(args []string) int {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		printUsage(os.Stderr)
		return 2
	}
}

// demoFlags holds the parsed demo parameters.
type demoFlags struct {
	seed           int64
	loss           float64
	reorderWindow  int
	maxBufferBytes int64
	consumerDelay  int64
	segmentMS      int64
}

func parseDemoFlags(args []string) (demoFlags, *flag.FlagSet, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	var d demoFlags
	fs.Int64Var(&d.seed, "seed", 42, "deterministic fixture seed")
	fs.Float64Var(&d.loss, "loss", 0.03, "simulated packet loss fraction [0,1)")
	fs.IntVar(&d.reorderWindow, "reorder-window", 64, "bounded reorder window in packets")
	fs.Int64Var(&d.maxBufferBytes, "max-buffer-bytes", 1<<20, "peak byte budget across queues")
	fs.Int64Var(&d.consumerDelay, "consumer-delay-ms", 8, "per-packet consumer delay in milliseconds")
	fs.Int64Var(&d.segmentMS, "segment-ms", 500, "output segment duration in milliseconds")
	if err := fs.Parse(args); err != nil {
		return d, fs, err
	}
	if d.loss < 0 || d.loss >= 1 {
		return d, fs, fmt.Errorf("loss must be within [0,1), got %f", d.loss)
	}
	if d.segmentMS <= 0 || d.consumerDelay < 0 || d.maxBufferBytes <= 0 {
		return d, fs, fmt.Errorf("segment-ms, consumer-delay-ms and max-buffer-bytes must be positive/non-negative")
	}
	return d, fs, nil
}

func runDemo(args []string) int {
	d, _, err := parseDemoFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid arguments: %v\n", err)
		return 2
	}
	spec := testdata.DefaultSpec(d.seed)
	spec.LossRate = d.loss
	// Deterministic weak-network faults so every demo shows located,
	// isolated failures without poisoning the whole run.
	spec.Corruptions = []testdata.Corruption{
		{Track: spec.AudioSSRC, SeqIndex: 5, Kind: testdata.CorruptTruncated},
		{Track: 0, SeqIndex: 0, Kind: testdata.CorruptBadRTCP},
		{Track: spec.AudioSSRC, SeqIndex: 11, Kind: testdata.CorruptDuplicateConflict},
	}
	cfg := pipeline.Config{
		MaxBufferBytes:    d.maxBufferBytes,
		MaxBufferPackets:  256,
		ReorderWindow:     d.reorderWindow,
		ConsumerTimeoutNS: 2_000_000_000,
	}
	opts := pipeline.RunOptions{
		SegmentNS:       d.segmentMS * 1_000_000,
		ConsumerDelayNS: d.consumerDelay * 1_000_000,
		Audio:           pipeline.DefaultAudioSpec(),
	}
	runner, merr := pipeline.NewRunner(cfg, opts)
	if merr != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", merr)
		return 2
	}
	fx := testdata.Generate(spec)
	res, merr := runner.Run(context.Background(), fx)
	if merr != nil {
		return reportFatal(merr)
	}
	// The 20% fixed-overhead allowance is part of the memory contract.
	th := metrics.DefaultThresholds()
	th.MaxBufferBytes = d.maxBufferBytes + d.maxBufferBytes/5
	summary := metrics.Evaluate(res, fx.MediaDatagrams(), th)
	metrics.WriteText(os.Stdout, summary)
	printLocatedFailures(res)
	if !summary.Passed {
		return 1
	}
	return 0
}

// reportFatal maps a structured fatal error onto exit code 3 (cancel/timeout)
// or 2 (invalid input).
func reportFatal(merr *mediaerr.Error) int {
	fmt.Fprintf(os.Stderr, "fatal: %v\n", merr)
	switch merr.Kind {
	case mediaerr.KindCanceled, mediaerr.KindTimeout:
		return 3
	case mediaerr.KindInvalidInput:
		return 2
	default:
		return 3
	}
}

func printUsage(w *os.File) {
	fmt.Fprintf(w, "usage: mediapipeline <demo|verify> [flags]\n")
	fmt.Fprintf(w, "  demo   run the weak-network pipeline and print quantified metrics\n")
	fmt.Fprintf(w, "  verify run reproducibility, round-trip and boundary assertions\n")
}
