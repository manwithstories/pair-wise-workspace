// Command avcheck analyses a Matroska/WebM recording end to end: EBML parse,
// track dependency graph, RTP jitter/repair, audio-anchored sync and manifest
// emission. It is a terminal-only tool with no external services.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"avcheck/internal/ebml"
	"avcheck/internal/fixture"
	"avcheck/internal/media"
	"avcheck/internal/pipeline"
	"avcheck/internal/report"
	"avcheck/internal/transport"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `avcheck - audio/video alignment checker

usage:
  avcheck demo  --case <name> [--seed N] [--target-bps N] [--memory-bytes N] [--out DIR]
  avcheck check --in <file>   [--seed N] [--target-bps N] [--memory-bytes N] [--out DIR]
  avcheck cases

demo cases:
  clean, burst-reorder-slow, truncated-vint, double-loss, oversize-element,
  unknown-track, dependency-cycle
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return media.ExitInternal
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	case "check":
		return runCheck(args[1:], stdout, stderr)
	case "cases":
		for _, c := range pipeline.Cases {
			fmt.Fprintln(stdout, c)
		}
		return media.ExitOK
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return media.ExitOK
	default:
		fmt.Fprintf(stderr, "avcheck: unknown command %q\n\n%s", args[0], usage)
		return media.ExitInternal
	}
}

func newFlagSet(name string, stdout, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func runDemo(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("demo", stdout, stderr)
	caseName := fs.String("case", "clean", "demo case name")
	seed := fs.Int64("seed", 731, "deterministic fixture seed")
	target := fs.Int64("target-bps", 900_000, "target bitrate for the report")
	memory := fs.Int64("memory-bytes", transport.HardMaxBufferBytes, "memory budget for queued payload")
	out := fs.String("out", "", "output directory (required)")
	maxElement := fs.Int64("max-element-bytes", ebml.DefaultLimits().MaxElementSize, "hard cap for one element body")
	if err := fs.Parse(args); err != nil {
		return media.ExitInternal
	}
	if *out == "" {
		fmt.Fprintln(stderr, "avcheck demo: --out is required")
		return media.ExitInternal
	}
	sc, err := pipeline.ScenarioFor(pipeline.Case(*caseName), *seed, *target, *memory)
	if err != nil {
		fmt.Fprintf(stderr, "avcheck demo: %v\n", err)
		return media.ExitInternal
	}
	sample, err := fixture.Build(sc.Spec)
	if err != nil {
		fmt.Fprintf(stderr, "avcheck demo: build fixture: %v\n", err)
		return media.ExitInternal
	}
	data := sample.Bytes
	if sc.Corrupt != nil {
		data, _, err = sc.Corrupt(data)
		if err != nil {
			fmt.Fprintf(stderr, "avcheck demo: corrupt: %v\n", err)
			return media.ExitInternal
		}
	}
	cfg := pipeline.DefaultConfig()
	cfg.Seed = sc.Spec.Seed
	cfg.TargetBps = sc.TargetBps
	cfg.MemoryBytes = sc.MemoryBytes
	cfg.Link = sc.Link
	cfg.Limits.MaxElementSize = *maxElement
	return execute(cfg, data, *out, stdout, stderr)
}

func runCheck(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("check", stdout, stderr)
	in := fs.String("in", "", "input Matroska/WebM file (required)")
	seed := fs.Int64("seed", 731, "seed for the simulated link")
	target := fs.Int64("target-bps", 900_000, "target bitrate for the report")
	memory := fs.Int64("memory-bytes", transport.HardMaxBufferBytes, "memory budget for queued payload")
	out := fs.String("out", "", "output directory (required)")
	if err := fs.Parse(args); err != nil {
		return media.ExitInternal
	}
	if *in == "" || *out == "" {
		fmt.Fprintln(stderr, "avcheck check: --in and --out are required")
		return media.ExitInternal
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintf(stderr, "avcheck check: read: %v\n", err)
		return media.ExitInternal
	}
	cfg := pipeline.DefaultConfig()
	cfg.Seed = *seed
	cfg.TargetBps = *target
	cfg.MemoryBytes = *memory
	return execute(cfg, data, *out, stdout, stderr)
}

func execute(cfg pipeline.Config, data []byte, outDir string, stdout, stderr io.Writer) int {
	runner, err := pipeline.New(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "avcheck: config: %v\n", err)
		return media.ExitInternal
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	start := time.Now()
	res, err := runner.RunFromBytes(ctx, data, outDir)
	if err != nil {
		var ce *pipeline.CanceledError
		if errors.As(err, &ce) {
			fmt.Fprintln(stderr, "avcheck: canceled")
			return media.ExitInternal
		}
		fmt.Fprintf(stderr, "avcheck: %v\n", err)
		return media.ExitInternal
	}
	elapsed := time.Since(start)
	if res.Committed && res.Manifest != nil {
		printManifest(stdout, res.Manifest, res.ManifestPath, elapsed)
		return media.ExitOK
	}
	printDiagnostics(stdout, res)
	return media.ExitCorrupt
}

// printManifest prints the machine-readable metric line plus a short header.
// The metric line is exactly what the acceptance checks grep for.
func printManifest(w io.Writer, m *report.Manifest, path string, elapsed time.Duration) {
	fmt.Fprintf(w, "status=committed manifest=%s elapsed=%s\n", path, elapsed.Round(time.Millisecond))
	fmt.Fprintln(w, m.SummaryLine())
	for _, t := range m.Tracks {
		fmt.Fprintf(w, "track=%d kind=%s samples=%d gaps=%d recovered=%d monotonic=%t\n",
			t.Track, t.Kind, t.Samples, t.Gaps, t.Recovered, t.Monotonic)
	}
}

// printDiagnostics reports why no manifest was committed, including the exact
// fault offset the contract asks for.
func printDiagnostics(w io.Writer, res *pipeline.RunResult) {
	fmt.Fprintf(w, "status=not_committed diagnostics=%s\n", res.ManifestPath)
	if res.Faults == nil || res.Faults.Empty() {
		fmt.Fprintln(w, "fault=<none>")
		return
	}
	first := res.Faults.List[0]
	fmt.Fprintf(w, "fault=%s offset=%d level=%s\n", first.Code, first.Offset, first.Level)
	for _, f := range res.Faults.List {
		fmt.Fprintf(w, "detail: %s\n", f.String())
	}
	_ = filepath.Base(res.ManifestPath)
}
