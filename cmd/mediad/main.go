// Command mediad is the terminal acceptance entry point. It has two verbs:
//
//	mediad demo   --seed N --case NAME --out DIR
//	mediad verify DIR_A DIR_B
//
// demo synthesises the requested fixture set inside --out, runs the delivery
// pipeline and prints the acceptance tokens. verify compares two deliveries by
// hash.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/asticode/go-astiav"

	"mediad/internal/fixtures"
	"mediad/internal/manifest"
	"mediad/internal/media"
	"mediad/internal/pipeline"
)

// exit codes follow the acceptance contract.
const (
	exitOK        = 0
	exitInvalidIn = 2 // invalid time base / manifest
	exitUnrecover = 3 // unrecoverable media
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: mediad <demo|verify> [options]")
		return exitInvalidIn
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, "usage: mediad demo --seed N --case NAME --out DIR | mediad verify DIR_A DIR_B")
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown verb %q\n", args[0])
		return exitInvalidIn
	}
}

// demoOptions mirrors the documented command line.
type demoOptions struct {
	Seed int64
	Case string
	Out  string
}

func parseDemo(args []string) (*demoOptions, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	o := &demoOptions{Case: fixtures.CaseClean}
	fs.Int64Var(&o.Seed, "seed", 41, "fixture seed")
	fs.StringVar(&o.Case, "case", fixtures.CaseClean, "fixture case: clean, late-wrap, truncated, invalid-base")
	fs.StringVar(&o.Out, "out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if o.Out == "" {
		return nil, errors.New("--out is required")
	}
	return o, nil
}

func runDemo(args []string, stdout, stderr *os.File) int {
	opts, err := parseDemo(args)
	if err != nil {
		fmt.Fprintln(stderr, "demo:", err)
		return exitInvalidIn
	}
	// Suppress libav's chatty transcoding logs; the report is the interface.
	astiavQuiet()
	stats, err := demo(opts, stdout)
	if err != nil {
		return reportError(err, stdout, stderr)
	}
	printStats(stdout, stats)
	if stats.ClippedSamples != 0 {
		fmt.Fprintln(stderr, "warning: clipped samples detected")
	}
	return exitOK
}

// demo runs one fixture set end to end.
func demo(opts *demoOptions, stdout *os.File) (*pipeline.Stats, error) {
	fixtureRoot := filepath.Join(opts.Out, "fixtures")
	outDir := filepath.Join(opts.Out, "delivery")
	set, err := fixtures.Build(fixtures.Spec{Root: fixtureRoot, Seed: opts.Seed, Case: opts.Case})
	if err != nil {
		return nil, err
	}
	// The invalid-base case must fail during manifest parsing, before any
	// media is opened.
	m, err := manifest.ParseManifestFile(set.Manifest)
	if err != nil {
		return nil, err
	}
	edits, err := manifest.ParseEditsFile(set.Edits)
	if err != nil {
		return nil, err
	}
	stats, err := pipeline.Run(pipeline.Options{
		MediaRoot: set.Root,
		Manifest:  m,
		Edits:     edits,
		OutDir:    outDir,
	})
	if err != nil {
		return nil, err
	}
	return stats, nil
}

func reportError(err error, stdout, stderr *os.File) int {
	code := "UNKNOWN"
	if me, ok := media.AsMediaError(err); ok {
		code = string(me.Code)
	}
	fmt.Fprintf(stdout, "error_code=%s\n", code)
	fmt.Fprintf(stderr, "error: %v\n", err)
	return media.ExitCode(err)
}

// printStats emits the acceptance token line. Every value is stable across
// repeated runs on identical input.
func printStats(w *os.File, s *pipeline.Stats) {
	pairs := []string{
		"target_kbps", media.FormatIntN(s.TargetKbps),
		"actual_kbps", media.FormatFloat(s.ActualKbps, 2),
		"bitrate_error_pct", media.FormatFloat(s.BitrateErrorPct, 2),
		"integrated_lufs", media.FormatFloat(s.IntegratedLUFS, 2),
		"true_peak_dbtp", media.FormatFloat(s.TruePeakDBTP, 2),
		"clipped_samples", media.FormatInt(s.ClippedSamples),
		"silent_seconds", media.FormatFloat(s.SilentSeconds, 3),
		"drift_ppm", media.FormatFloat(s.DriftPPM, 3),
		"drift_anomaly", boolToken(s.DriftAnomaly),
		"late_events", media.FormatIntN(s.LateEvents),
		"replayed_events", media.FormatIntN(s.ReplayedEvents),
		"manifest_alignment_us", media.FormatFloat(s.ManifestAlignUS, 3),
		"damaged_ranges", media.FormatIntN(s.DamagedRanges),
		"duration_seconds", media.FormatFloat(s.DurationSeconds, 3),
		"output_sha256", s.OutputSHA256,
		"output_path", s.OutputPath,
	}
	for _, d := range s.Damaged {
		pairs = append(pairs, fmt.Sprintf("damaged_%s_%s", d.Camera, d.Track),
			fmt.Sprintf("%d-%d@%s", d.StartRaw, d.EndRaw, d.Reason))
	}
	fmt.Fprintln(w, media.FormatKV(pairs...))
}

func boolToken(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func runVerify(args []string, stdout, stderr *os.File) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: mediad verify DIR_A DIR_B")
		return exitInvalidIn
	}
	a, err := deliveryFile(args[0])
	if err != nil {
		return reportError(err, stdout, stderr)
	}
	b, err := deliveryFile(args[1])
	if err != nil {
		return reportError(err, stdout, stderr)
	}
	hashA, err := pipeline.HashFile(a)
	if err != nil {
		return reportError(err, stdout, stderr)
	}
	hashB, err := pipeline.HashFile(b)
	if err != nil {
		return reportError(err, stdout, stderr)
	}
	same := hashA == hashB
	fmt.Fprintln(stdout, media.FormatKV(
		"verify", map[bool]string{true: "identical", false: "different"}[same],
		"sha256_a", hashA,
		"sha256_b", hashB,
	))
	if !same {
		fmt.Fprintln(stderr, "verify: hashes differ")
		return exitUnrecover
	}
	return exitOK
}

// deliveryFile resolves the delivery inside a demo output directory.
func deliveryFile(dir string) (string, error) {
	candidates := []string{
		filepath.Join(dir, "delivery.m4a"),
		filepath.Join(dir, "delivery", "delivery.m4a"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, nil
		}
	}
	return "", media.NewError(media.ErrIO, "no delivery found under %s", dir)
}

// astiavQuiet lowers libav logging to errors only.
func astiavQuiet() { astiav.SetLogLevel(astiav.LogLevelError) }
