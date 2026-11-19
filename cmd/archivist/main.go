package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"archivearchivist/internal/pipeline"
	"archivearchivist/internal/recover"
	"archivearchivist/internal/testkit"
)

const (
	exitOK            = 0
	exitInvalid       = 2
	exitUnrecoverable = 3
	exitInternal      = 4
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitInvalid
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "generate":
		return cmdGenerate(rest, stdout, stderr)
	case "inspect":
		return cmdAnalyze(rest, stdout, stderr, "inspect")
	case "recover":
		return cmdAnalyze(rest, stdout, stderr, "recover")
	case "bench":
		return cmdAnalyze(rest, stdout, stderr, "bench")
	case "-h", "--help", "help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
		usage(stderr)
		return exitInvalid
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `archivist - archive timeline inspection and recovery

  archivist generate --out DIR [--seed N] [--video-frames N]
  archivist inspect  --input timeline.json [--capture stream.rtp] [--json FILE]
  archivist recover  --input timeline.json [--capture stream.rtp] [--iterations N] [--json FILE]
  archivist bench    --input timeline.json [--capture stream.rtp] [--iterations N] [--json FILE]

Exit codes: 0 done, 2 invalid arguments or input, 3 unrecoverable segment, 4 internal error.
`)
}

func cmdGenerate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "output directory for the fixture")
	seed := fs.Int64("seed", 20261120, "deterministic fixture seed")
	frames := fs.Int("video-frames", 240, "number of video frames")
	if err := fs.Parse(args); err != nil {
		return exitInvalid
	}
	if *out == "" {
		fmt.Fprintln(stderr, "generate: --out is required")
		return exitInvalid
	}
	fx, err := testkit.Generate(testkit.Config{Seed: *seed, OutDir: *out, VideoFrames: *frames})
	if err != nil {
		fmt.Fprintf(stderr, "generate: %v\n", err)
		return exitInvalid
	}
	fmt.Fprintf(stdout, "generated fixture in %s\n", fx.Dir)
	fmt.Fprintf(stdout, "  timeline      %s\n", fx.Timeline)
	fmt.Fprintf(stdout, "  capture       %s\n", fx.Capture)
	fmt.Fprintf(stdout, "  cycle variant %s\n", fx.CycleTimeline)
	fmt.Fprintf(stdout, "  tracks        %d\n", len(fx.Manifest.Tracks))
	fmt.Fprintf(stdout, "  corrupt bytes [%d,%d] on %s\n",
		fx.Manifest.CorruptByteStart, fx.Manifest.CorruptByteEnd, fx.Manifest.CorruptTrack)
	fmt.Fprintf(stdout, "  lost packets  %d, late %d, duplicates %d\n",
		len(fx.Manifest.LostSequences), len(fx.Manifest.LateSequences), fx.Manifest.DuplicateCount)
	fmt.Fprintf(stdout, "  timestamp wraps %d\n", fx.Manifest.TimestampWraps)
	return exitOK
}

func cmdAnalyze(args []string, stdout, stderr io.Writer, name string) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "timeline JSON file")
	capture := fs.String("capture", "", "RTP capture file")
	jsonOut := fs.String("json", "", "write the full result as JSON to this file")
	iterations := fs.Int("iterations", 1, "repeat count; bench and recover clamp to at least 30")
	window := fs.Int("window", 0, "RTP receive window in packets (default from timeline)")
	maxErr := fs.Int64("max-snap-us", recover.DefaultConfig().MaxErrorUS, "largest accepted keyframe snap error in microseconds")
	if err := fs.Parse(args); err != nil {
		return exitInvalid
	}
	if *input == "" {
		fmt.Fprintf(stderr, "%s: --input is required\n", name)
		return exitInvalid
	}
	if _, err := os.Stat(*input); err != nil {
		fmt.Fprintf(stderr, "%s: cannot read %s: %v\n", name, *input, err)
		return exitInvalid
	}

	inputSpec := pipeline.Input{TimelinePath: *input, CapturePath: *capture}
	opts := pipeline.Options{
		Window:  *window,
		Recover: recover.Config{KeyframeSearch: recover.DefaultConfig().KeyframeSearch, MaxErrorUS: *maxErr},
	}

	// bench always repeats. recover repeats too when asked, so the documented
	// acceptance command produces latency figures, but keeps its normal report.
	repeat := name == "bench" || (*iterations > 1 && name == "recover")
	if repeat {
		if *iterations < 30 {
			fmt.Fprintf(stderr, "%s: --iterations %d raised to the required minimum of 30\n", name, *iterations)
		}
		res, err := pipeline.Benchmark(inputSpec, opts, *iterations)
		if err != nil {
			return fail(stderr, name, &invalidInput{err})
		}
		if name == "bench" {
			printBench(stdout, res)
		} else {
			printAnalysis(stdout, res.Analysis, name)
		}
		if *jsonOut != "" {
			var doc any
			if name == "bench" {
				doc = pipeline.BenchMarks{Command: name, Bench: res, Metrics: res.Analysis.Metrics, Faults: res.Analysis.Faults}
			} else {
				// recover keeps the full analysis document, with the latency
				// percentiles folded into metrics.
				doc = res.Analysis
			}
			if err := writeJSON(*jsonOut, doc); err != nil {
				return fail(stderr, name, err)
			}
		}
		return res.Analysis.Faults.ExitCode()
	}

	a, err := pipeline.Run(inputSpec, opts)
	if err != nil {
		return fail(stderr, name, &invalidInput{err})
	}
	if *jsonOut != "" {
		if err := writeJSON(*jsonOut, a); err != nil {
			return fail(stderr, name, err)
		}
	}
	printAnalysis(stdout, a, name)
	return a.Faults.ExitCode()
}

func fail(stderr io.Writer, cmd string, err error) int {
	var invalid *invalidInput
	if errors.As(err, &invalid) {
		fmt.Fprintf(stderr, "%s: %v\n", cmd, err)
		return exitInvalid
	}
	fmt.Fprintf(stderr, "%s: %v\n", cmd, err)
	return exitInternal
}

// invalidInput marks errors caused by user input rather than a bug.
type invalidInput struct{ err error }

func (e *invalidInput) Error() string { return e.err.Error() }
func (e *invalidInput) Unwrap() error { return e.err }

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
