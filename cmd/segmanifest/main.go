package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"segmanifest/internal/align"
	"segmanifest/internal/fixture"
	"segmanifest/internal/ladder"
	"segmanifest/internal/manifest"
	"segmanifest/internal/report"
	"segmanifest/internal/verify"
)

const (
	exitOK      = 0
	exitVerify  = 1
	exitInput   = 2
	exitDecode  = 3
	exitSubtask = 4
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitInput
	}
	switch args[0] {
	case "gen":
		return cmdGen(args[1:], stdout, stderr)
	case "plan":
		return cmdPlan(args[1:], stdout, stderr)
	case "emit":
		return cmdEmit(args[1:], stdout, stderr)
	case "verify":
		return cmdVerify(args[1:], stdout, stderr)
	case "stats":
		return cmdStats(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "segmanifest: unknown subcommand %q\n", args[0])
		usage(stderr)
		return exitInput
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: segmanifest <gen|plan|emit|verify|stats> [flags]")
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func cmdGen(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("gen", stderr)
	out := fs.String("out", "", "fixture output directory")
	seed := fs.Int64("seed", 1, "deterministic fixture seed")
	reps := fs.Int("representations", 3, "number of representations")
	segments := fs.Int("segments", 120, "segments per representation")
	offsetMS := fs.Float64("offset-ms", 0, "audio start offset in milliseconds")
	jitterMS := fs.Float64("jitter-ms", 0, "boundary jitter scale in milliseconds")
	holes := fs.Int("holes", 0, "segments removed per representation")
	version := fs.Uint64("version", 1, "period version")
	target := fs.Duration("target", 4*time.Second, "nominal segment duration")
	if err := fs.Parse(args); err != nil {
		return exitInput
	}
	if *out == "" {
		fmt.Fprintln(stderr, "segmanifest gen: --out is required")
		return exitInput
	}
	spec, err := fixture.Generate(fixture.Options{
		Seed: *seed, Representations: *reps, Segments: *segments,
		TargetUS: target.Microseconds(), JitterUS: msToUS(*jitterMS),
		AudioOffsetUS: msToUS(*offsetMS), Holes: *holes, Version: *version,
	})
	if err != nil {
		fmt.Fprintln(stderr, "segmanifest gen:", err)
		return exitInput
	}
	if err := fixture.Write(*out, spec); err != nil {
		fmt.Fprintln(stderr, "segmanifest gen:", err)
		return exitSubtask
	}
	generatedSegments := 0
	for _, rep := range spec.Representations {
		generatedSegments += len(rep.Segments)
	}
	fmt.Fprintf(stdout, "generated=%s representations=%d segments=%d\n", *out, len(spec.Representations), generatedSegments)
	return exitOK
}

func cmdPlan(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("plan", stderr)
	in := fs.String("in", "", "fixture input directory")
	out := fs.String("out", "", "manifest output file")
	target := fs.Duration("target", 4*time.Second, "target segment duration")
	tolerance := fs.Duration("tolerance", 2*time.Millisecond, "maximum boundary tolerance")
	template := fs.String("template", "", "segment path template override")
	window := fs.Int("window", 0, "keep only the most recent N segments")
	mergeDirs := fs.String("merge", "", "comma-separated newer fixture periods to merge")
	if err := fs.Parse(args); err != nil {
		return exitInput
	}
	if *in == "" || *out == "" {
		fmt.Fprintln(stderr, "segmanifest plan: --in and --out are required")
		return exitInput
	}
	spec, err := fixture.Load(*in)
	if err != nil {
		fmt.Fprintln(stderr, "segmanifest plan:", err)
		return exitInput
	}
	periods := []*fixture.Spec{spec}
	for _, dir := range fixture.SplitPaths(*mergeDirs) {
		extra, err := fixture.Load(dir)
		if err != nil {
			fmt.Fprintln(stderr, "segmanifest plan:", err)
			return exitInput
		}
		periods = append(periods, extra)
	}
	if len(periods) > 1 {
		spec, err = fixture.Merge(periods...)
		if err != nil {
			fmt.Fprintln(stderr, "segmanifest plan:", err)
			return exitInput
		}
	}
	targetUS, toleranceUS := target.Microseconds(), tolerance.Microseconds()
	if err := ladder.ValidateConfig(targetUS, toleranceUS); err != nil {
		fmt.Fprintln(stderr, "segmanifest plan:", err)
		return exitInput
	}
	m, err := spec.ToManifest(targetUS, toleranceUS)
	if err != nil {
		fmt.Fprintln(stderr, "segmanifest plan:", err)
		return exitInput
	}
	if *template != "" {
		m.Template = *template
	}
	if _, err := manifest.ParseTemplate(m.Template); err != nil {
		fmt.Fprintln(stderr, "segmanifest plan:", err)
		return exitInput
	}
	if *window > 0 {
		if err := manifest.RollWindow(m, *window); err != nil {
			fmt.Fprintln(stderr, "segmanifest plan:", err)
			return exitInput
		}
	}
	grid, err := ladder.BuildGrid(m.Representations, targetUS, toleranceUS)
	if err != nil {
		fmt.Fprintln(stderr, "segmanifest plan:", err)
		return exitInput
	}
	if err := align.Align(m, grid); err != nil {
		fmt.Fprintln(stderr, "segmanifest plan:", err)
		return exitSubtask
	}
	if err := writeManifestAtomic(*out, m); err != nil {
		fmt.Fprintln(stderr, "segmanifest plan:", err)
		return exitSubtask
	}
	report.PrintPlan(stdout, m, len(grid.Points))
	return exitOK
}

func cmdEmit(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("emit", stderr)
	path := fs.String("manifest", "", "manifest file")
	repID := fs.String("rep", "", "representation ID")
	out := fs.String("out", "", "path list output file (stdout when omitted)")
	if err := fs.Parse(args); err != nil {
		return exitInput
	}
	if *path == "" || *repID == "" {
		fmt.Fprintln(stderr, "segmanifest emit: --manifest and --rep are required")
		return exitInput
	}
	m, code, err := loadManifest(*path)
	if err != nil {
		fmt.Fprintln(stderr, "segmanifest emit:", err)
		return code
	}
	rep, ok := manifest.FindRepresentation(m, *repID)
	if !ok {
		fmt.Fprintf(stderr, "segmanifest emit: representation %q not found\n", *repID)
		return exitInput
	}
	template, err := manifest.ParseTemplate(m.Template)
	if err != nil {
		fmt.Fprintln(stderr, "segmanifest emit:", err)
		return exitInput
	}
	lines := make([]string, 0, len(rep.Segments))
	seen := make(map[string]struct{}, len(rep.Segments))
	for _, segment := range rep.Segments {
		line, err := template.Expand(rep.ID, rep.Bandwidth, segment.Number, segment.StartUS)
		if err != nil {
			fmt.Fprintln(stderr, "segmanifest emit:", err)
			return exitInput
		}
		if _, ok := seen[line]; ok {
			fmt.Fprintf(stderr, "segmanifest emit: template.duplicate_path: %q is produced more than once\n", line)
			return exitInput
		}
		seen[line] = struct{}{}
		lines = append(lines, line)
	}
	if *out == "" {
		for _, line := range lines {
			fmt.Fprintln(stdout, line)
		}
		return exitOK
	}
	if err := writeLinesAtomic(*out, lines); err != nil {
		fmt.Fprintln(stderr, "segmanifest emit:", err)
		return exitSubtask
	}
	return exitOK
}

func cmdVerify(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("verify", stderr)
	path := fs.String("manifest", "", "manifest file")
	asJSON := fs.Bool("json", false, "emit JSON")
	strict := fs.Bool("strict", false, "fail when drift exceeds the threshold")
	maxDriftMS := fs.Float64("max-drift-ms", -1, "strict maximum drift in milliseconds (default: manifest tolerance)")
	if err := fs.Parse(args); err != nil {
		return exitInput
	}
	if *path == "" {
		fmt.Fprintln(stderr, "segmanifest verify: --manifest is required")
		return exitInput
	}
	m, code, err := loadManifest(*path)
	if err != nil {
		fmt.Fprintln(stderr, "segmanifest verify:", err)
		return code
	}
	result := verify.Verify(m)
	verify.SortProblems(&result)
	if err := report.PrintVerify(stdout, result, *asJSON); err != nil {
		fmt.Fprintln(stderr, "segmanifest verify:", err)
		return exitSubtask
	}
	for _, problem := range result.Problems {
		fmt.Fprintln(stderr, "segmanifest verify:", problem)
	}
	if !result.OK {
		return exitVerify
	}
	if *strict {
		threshold := float64(m.ToleranceUS) / 1000
		if *maxDriftMS >= 0 {
			threshold = *maxDriftMS
		}
		overall := report.Overall(m)
		if overall.MaxDriftMS > threshold {
			fmt.Fprintf(stderr, "segmanifest verify: max drift %.3fms exceeds %.3fms\n", overall.MaxDriftMS, threshold)
			return exitVerify
		}
	}
	return exitOK
}

func cmdStats(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("stats", stderr)
	path := fs.String("manifest", "", "manifest file")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitInput
	}
	if *path == "" {
		fmt.Fprintln(stderr, "segmanifest stats: --manifest is required")
		return exitInput
	}
	m, code, err := loadManifest(*path)
	if err != nil {
		fmt.Fprintln(stderr, "segmanifest stats:", err)
		return code
	}
	if err := report.PrintStats(stdout, m, *asJSON); err != nil {
		fmt.Fprintln(stderr, "segmanifest stats:", err)
		return exitSubtask
	}
	return exitOK
}

func loadManifest(path string) (*manifest.Manifest, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, exitInput, err
	}
	defer file.Close()
	m, err := manifest.Decode(file)
	if err != nil {
		return nil, exitDecode, err
	}
	return m, exitOK, nil
}

func writeManifestAtomic(path string, m *manifest.Manifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".manifest-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := manifest.Encode(m, file); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeLinesAtomic(path string, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".paths-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	for _, line := range lines {
		if _, err := fmt.Fprintln(file, line); err != nil {
			file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func msToUS(ms float64) int64 {
	return int64(math.Round(ms * 1000))
}
