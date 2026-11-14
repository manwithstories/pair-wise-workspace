// Command tsaudit generates, audits, and slices MPEG-TS test streams.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"tsaudit/internal/report"
	"tsaudit/internal/scan"
	"tsaudit/internal/segment"
	"tsaudit/internal/synth"
)

const (
	exitOK          = 0
	exitInput       = 1
	exitFailOn      = 2
	exitInvalidArgs = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return exitInvalidArgs
	}
	switch args[0] {
	case "gen":
		return runGen(args[1:], stdout, stderr)
	case "scan":
		return runScan(args[1:], stdout, stderr)
	case "slice":
		return runSlice(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n", args[0])
		printUsage(stderr)
		return exitInvalidArgs
	}
}

func runGen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "output transport stream")
	programs := fs.Int("programs", 3, "number of programs")
	packets := fs.Int("packets", 400000, "target packet count")
	bumps := fs.Int("bumps", 5, "PAT version changes")
	lossBursts := fs.Int("loss-bursts", 3, "loss clusters")
	dups := fs.Int("dup", 12, "duplicate packets")
	seed := fs.Int64("seed", 7, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitInvalidArgs
	}
	if fs.NArg() != 0 || *out == "" {
		fmt.Fprintln(stderr, "gen: --out is required and positional arguments are not accepted")
		return exitInvalidArgs
	}
	cfg := synth.Config{
		Programs: *programs, Packets: *packets, Bumps: *bumps,
		LossBursts: *lossBursts, Dups: *dups, Seed: *seed,
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "gen: %v\n", err)
		return exitInvalidArgs
	}
	if err := writeGenerated(*out, cfg); err != nil {
		fmt.Fprintf(stderr, "gen: %v\n", err)
		return exitInput
	}
	fmt.Fprintf(stdout, "generated %s programs=%d packets=%d bumps=%d loss_bursts=%d dup=%d seed=%d\n",
		*out, *programs, *packets, *bumps, *lossBursts, *dups, *seed)
	return exitOK
}

func writeGenerated(path string, cfg synth.Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tsaudit-gen-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := synth.Generate(tmp, cfg); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func runScan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "transport stream input")
	format := fs.String("format", "text", "text or json")
	out := fs.String("out", "", "optional report output file")
	pmtTimeout := fs.Int64("pmt-timeout", 2000, "PMT pending timeout in packets")
	minBytes := fs.Int64("min-segment-bytes", 65536, "minimum planned segment size")
	var failOn stringsFlag
	fs.Var(&failOn, "fail-on", "metric threshold, e.g. cc_jumps or lost_packets>=1")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitInvalidArgs
	}
	if fs.NArg() != 0 || *input == "" {
		fmt.Fprintln(stderr, "scan: --input is required and positional arguments are not accepted")
		return exitInvalidArgs
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintln(stderr, "scan: --format must be text or json")
		return exitInvalidArgs
	}
	if *pmtTimeout < 0 || *minBytes < 1 {
		fmt.Fprintln(stderr, "scan: --pmt-timeout must be >= 0 and --min-segment-bytes must be > 0")
		return exitInvalidArgs
	}
	conditions, err := parseFailOn(failOn)
	if err != nil {
		fmt.Fprintf(stderr, "scan: %v\n", err)
		return exitInvalidArgs
	}
	rep, err := scan.File(*input, scan.Options{PMTTimeout: *pmtTimeout, MinSegmentBytes: *minBytes})
	if err != nil {
		fmt.Fprintf(stderr, "scan: %v\n", err)
		return exitInput
	}
	if *out != "" {
		if err := writeReport(*out, rep, *format); err != nil {
			fmt.Fprintf(stderr, "scan: %v\n", err)
			return exitInput
		}
		if err := rep.RenderText(stdout); err != nil {
			fmt.Fprintf(stderr, "scan: %v\n", err)
			return exitInput
		}
	} else if *format == "json" {
		if err := rep.RenderJSON(stdout); err != nil {
			fmt.Fprintf(stderr, "scan: %v\n", err)
			return exitInput
		}
	} else if err := rep.RenderText(stdout); err != nil {
		fmt.Fprintf(stderr, "scan: %v\n", err)
		return exitInput
	}
	if triggered(rep, conditions) {
		return exitFailOn
	}
	return exitOK
}

func runSlice(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("slice", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "transport stream input")
	outDir := fs.String("out-dir", "", "segment output directory")
	minBytes := fs.Int64("min-segment-bytes", 65536, "minimum segment size")
	pmtTimeout := fs.Int64("pmt-timeout", 2000, "PMT pending timeout in packets")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitInvalidArgs
	}
	if fs.NArg() != 0 || *input == "" || *outDir == "" {
		fmt.Fprintln(stderr, "slice: --input and --out-dir are required")
		return exitInvalidArgs
	}
	if *minBytes < 1 || *pmtTimeout < 0 {
		fmt.Fprintln(stderr, "slice: --min-segment-bytes must be > 0 and --pmt-timeout must be >= 0")
		return exitInvalidArgs
	}
	rep, err := scan.File(*input, scan.Options{PMTTimeout: *pmtTimeout, MinSegmentBytes: *minBytes, Input: *input})
	if err != nil {
		fmt.Fprintf(stderr, "slice: %v\n", err)
		return exitInput
	}
	manifest := segment.Manifest{
		Input: *input, MinBytes: *minBytes, Segments: rep.SliceManifest, Count: len(rep.SliceManifest),
	}
	if err := segment.Write(*input, *outDir, manifest); err != nil {
		fmt.Fprintf(stderr, "slice: %v\n", err)
		return exitInput
	}
	fmt.Fprintf(stdout, "segments=%d out_dir=%s manifest=%s\n", len(rep.SliceManifest), *outDir,
		filepath.Join(*outDir, "manifest.json"))
	for _, seg := range rep.SliceManifest {
		fmt.Fprintf(stdout, "  segment-%03d bytes=[%d,%d) snapshot=%d boundary=%s\n",
			seg.Index, seg.StartByte, seg.EndByte, seg.SnapshotID, seg.Boundary)
	}
	return exitOK
}

func writeReport(path string, rep *report.Report, format string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tsaudit-report-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if format == "json" {
		err = rep.RenderJSON(tmp)
	} else {
		err = rep.RenderText(tmp)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  tsaudit gen --out FILE [--programs N --packets N --bumps N --loss-bursts N --dup N --seed N]")
	fmt.Fprintln(w, "  tsaudit scan --input FILE [--format text|json] [--out FILE] [--fail-on METRIC[OP N]]")
	fmt.Fprintln(w, "  tsaudit slice --input FILE --out-dir DIR [--min-segment-bytes N]")
}
