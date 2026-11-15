// Command tsprobe scans MPEG-TS files, slices playable ranges, and generates fixtures.
package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"

	"tsprobe/internal/demux"
	"tsprobe/internal/fixture"
	"tsprobe/internal/report"
	"tsprobe/internal/slicer"
)

const (
	exitOK          = 0
	exitAnomalies   = 2
	exitUnreadable  = 3
	exitInvalidArgs = 4
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitInvalidArgs
	}
	switch args[0] {
	case "scan":
		return runScan(args[1:], stdout, stderr)
	case "gen":
		return runGen(args[1:], stdout, stderr)
	case "slice":
		return runSlice(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n", args[0])
		usage(stderr)
		return exitInvalidArgs
	}
}

func runScan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "input MPEG-TS file")
	jsonPath := fs.String("json", "", "write deterministic scan JSON to this path")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "scan: invalid arguments")
		return exitInvalidArgs
	}
	if *input == "" {
		fmt.Fprintln(stderr, "scan: --input is required")
		return exitInvalidArgs
	}
	result, err := demux.Scan(*input)
	if err != nil {
		fmt.Fprintf(stderr, "scan: %v\n", err)
		return exitUnreadable
	}
	rep := report.Build(result)
	if rep.PacketsTotal == 0 || rep.PAT.FirstVersion == nil {
		fmt.Fprintf(stderr, "scan: %s is not a recognisable MPEG-TS file\n", *input)
		return exitUnreadable
	}
	if *jsonPath != "" {
		if err := report.WriteJSON(*jsonPath, rep); err != nil {
			fmt.Fprintf(stderr, "scan: writing JSON: %v\n", err)
			return exitUnreadable
		}
	}
	report.PrintScan(stdout, rep)
	if rep.NeedsExitTwo() {
		return exitAnomalies
	}
	return exitOK
}

func runGen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "output TS file")
	programs := fs.Int("programs", 3, "number of synthetic programs")
	switches := fs.Int("switch", 0, "number of mid-stream PMT version switches")
	drop := fs.Float64("drop", 0, "target short-burst loss strength (0..1)")
	seed := fs.Int64("seed", 1, "deterministic random seed")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "gen: invalid arguments")
		return exitInvalidArgs
	}
	if *out == "" || *programs < 1 || *programs > 32 || *switches < 0 || *switches > 1000 ||
		*drop < 0 || *drop > 1 || math.IsNaN(*drop) || math.IsInf(*drop, 0) {
		fmt.Fprintln(stderr, "gen: invalid or missing parameters")
		return exitInvalidArgs
	}
	err := fixture.Generate(fixture.Options{Out: *out, Programs: *programs, Switches: *switches, Drop: *drop, Seed: *seed})
	if err != nil {
		fmt.Fprintf(stderr, "gen: %v\n", err)
		return exitUnreadable
	}
	fmt.Fprintf(stdout, "generated: %s programs=%d switches=%d drop=%.4f seed=%d\n", *out, *programs, *switches, *drop, *seed)
	return exitOK
}

func runSlice(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("slice", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "input MPEG-TS file")
	segmentMS := fs.Int("segment-ms", 2000, "target slice duration in milliseconds")
	outDir := fs.String("out-dir", "", "directory for TS slice files")
	indexPath := fs.String("index", "", "write slice index JSON to this path")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "slice: invalid arguments")
		return exitInvalidArgs
	}
	if *input == "" || *outDir == "" || *indexPath == "" || *segmentMS <= 0 {
		fmt.Fprintln(stderr, "slice: --input, --out-dir, --index and a positive --segment-ms are required")
		return exitInvalidArgs
	}
	result, err := demux.Scan(*input)
	if err != nil {
		fmt.Fprintf(stderr, "slice: %v\n", err)
		return exitUnreadable
	}
	rep := report.Build(result)
	if result.PacketsTotal == 0 || rep.PAT.FirstVersion == nil {
		fmt.Fprintf(stderr, "slice: %s is not a recognisable MPEG-TS file\n", *input)
		return exitUnreadable
	}
	idx, err := slicer.Build(result, slicer.Options{SegmentMS: *segmentMS, OutDir: *outDir})
	if err != nil {
		fmt.Fprintf(stderr, "slice: %v\n", err)
		return exitInvalidArgs
	}
	if idx.Slices.Total == 0 {
		fmt.Fprintln(stderr, "slice: no keyframe-based slices could be planned")
		return exitUnreadable
	}
	if err := slicer.WriteSegments(*input, &idx); err != nil {
		fmt.Fprintf(stderr, "slice: writing segments: %v\n", err)
		return exitUnreadable
	}
	if err := slicer.WriteIndex(*indexPath, idx); err != nil {
		fmt.Fprintf(stderr, "slice: writing index: %v\n", err)
		return exitUnreadable
	}
	fmt.Fprintf(stdout, "slices: total=%d corrupt=%d segment_ms=%d\n", idx.Slices.Total, idx.Slices.Corrupt, idx.SegmentMS)
	for _, s := range idx.Slices.Items {
		fmt.Fprintf(stdout, "  %06d packets=%d..%d ms=%d..%d corrupt=%t file=%s\n", s.Index, s.StartPacket, s.EndPacket, s.StartMS, s.EndMS, s.Corrupt, s.File)
	}
	if idx.Slices.Corrupt > 0 || rep.NeedsExitTwo() {
		return exitAnomalies
	}
	return exitOK
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: tsprobe <scan|slice|gen> [flags]")
	fmt.Fprintln(w, "  scan   --input file.ts [--json out/scan.json]")
	fmt.Fprintln(w, "  slice  --input file.ts --segment-ms 2000 --out-dir out/seg --index out/index.json")
	fmt.Fprintln(w, "  gen    --out testdata/room.ts --programs 3 --switch 4 --drop 0.01 --seed 7")
}
