// Command mediapipe is the terminal adapter for the broadcast remux pipeline.
// It generates self-contained fixtures, runs the quality gates, and prints one
// line per case plus aggregate facts. Exit codes follow the delivery contract:
// 0 success, 2 corrupt input, 3 unsupported mapping or beyond-horizon lateness,
// 4 recoverable interruption.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"mediapipe/internal/audit"
	"mediapipe/internal/mapping"
	"mediapipe/internal/media"
	"mediapipe/internal/timeline"
)

// stdout is swapped by tests to capture the terminal report.
var stdout io.Writer = os.Stdout

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 0
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "mediapipe: unknown command %q\n", args[0])
		usage()
		return 1
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mediapipe — broadcast stream-copy QC and remux

usage:
  mediapipe demo [--seed N] [--budget-mib N] [--slow-consumer DURATION]
                 [--target matroska|webm|fmp4] [--workdir DIR]

The demo generates its own fixtures (clean, late, truncated, resumable),
runs every QC gate, and prints one line per case:

  case=<name> target_bps=<n> actual_bps=<n> delta_pct=<f>
  payload_hash_mismatch=<n> crc_errors=<n> manifest_skew_ns=<n>
  buffer_peak_bytes=<n> replay_digest=<hex> corrupt_offset=<n>
  resume_digest_equal=<bool> exit=<n>

exit codes: 0 ok, 2 corrupt input, 3 unsupported mapping or lateness
beyond the retraction horizon, 4 recoverable interruption.
`)
}

type demoFlags struct {
	seed        int64
	budgetMiB   int64
	slow        time.Duration
	target      string
	workdir     string
	clusterSpan time.Duration
}

func parseDemoFlags(args []string) (demoFlags, error) {
	var f demoFlags
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Int64Var(&f.seed, "seed", 73, "fixture seed")
	fs.Int64Var(&f.budgetMiB, "budget-mib", 32, "queue budget in MiB")
	fs.DurationVar(&f.slow, "slow-consumer", 25*time.Millisecond, "per-packet consumer delay")
	fs.StringVar(&f.target, "target", "matroska", "output container: matroska|webm|fmp4")
	fs.StringVar(&f.workdir, "workdir", "", "working directory (default: temp dir)")
	fs.DurationVar(&f.clusterSpan, "cluster-span", 2*time.Second, "cluster/fragment span")
	fs.BoolVar(&verbose, "verbose", false, "print per-case notes and manifest details")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if f.budgetMiB <= 0 {
		return f, fmt.Errorf("budget-mib must be positive")
	}
	return f, nil
}

func runDemo(args []string) int {
	flags, err := parseDemoFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mediapipe demo: %v\n", err)
		return 1
	}
	target := mapping.Target(flags.target)
	if !target.Valid() {
		fmt.Fprintf(os.Stderr, "mediapipe demo: unknown target %q\n", flags.target)
		return 3
	}
	workdir := flags.workdir
	if workdir == "" {
		workdir, err = os.MkdirTemp("", "mediapipe-demo-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "mediapipe demo: %v\n", err)
			return 1
		}
		defer os.RemoveAll(workdir)
	}
	opts := jobOptions{
		Seed:        flags.seed,
		Budget:      flags.budgetMiB << 20,
		SlowDelay:   flags.slow,
		WorkDir:     workdir,
		Target:      target,
		ClusterSpan: media.FromDuration(flags.clusterSpan),
	}
	cases, exit := runAllCases(opts)
	for _, c := range cases {
		printCase(c)
	}
	if verbose {
		printSummary(cases, opts)
	}
	return exit
}

// printSummary folds the per-case facts into the delivery-level totals a
// sign-off sheet needs.
func printSummary(cases []demoCase, opts jobOptions) {
	var committed int
	var mismatch, crc int
	var peak int64
	for _, c := range cases {
		if c.Manifest.Committed {
			committed++
		}
		mismatch += c.Mismatch
		crc += c.CRCErrors
		if c.BufferPeak > peak {
			peak = c.BufferPeak
		}
	}
	fmt.Fprintf(stdout, "summary cases=%d committed=%d payload_hash_mismatch=%d crc_errors=%d buffer_peak_bytes=%d budget_bytes=%d target=%s seed=%d\n",
		len(cases), committed, mismatch, crc, peak, opts.Budget, opts.Target, opts.Seed)
}

func printCase(c demoCase) {
	fmt.Fprintf(stdout, "case=%s target_bps=%d actual_bps=%d delta_pct=%.4f "+
		"payload_hash_mismatch=%d crc_errors=%d manifest_skew_ns=%d "+
		"buffer_peak_bytes=%d replay_digest=%s corrupt_offset=%d "+
		"resume_digest_equal=%v exit=%d\n",
		c.Name, c.TargetBPS, c.ActualBPS, c.DeltaPct, c.Mismatch, c.CRCErrors,
		c.Skew, c.BufferPeak, c.Replay, c.CorruptOff, c.ResumeOK, c.ExitCode)
	if verbose && c.Note != "" {
		fmt.Fprintf(stdout, "  note[%s]=%s\n", c.Name, c.Note)
	}
}

// verbose enables the per-case explanation lines on stderr-adjacent stdout.
var verbose bool

// runAllCases builds fixtures, runs each scenario, and returns the exit code
// for the whole demo. Severity wins: a corrupt fixture is fatal (2), an
// unsupported mapping (3) outranks a recoverable interruption (4).
func runAllCases(opts jobOptions) ([]demoCase, int) {
	fixtures := buildFixtures(opts)
	var cases []demoCase
	worst := 0
	// Cases that intentionally fail report the exit code the contract expects
	// but must not fail the demo as a whole; `worst` only tracks unexpected
	// failures.
	add := func(c demoCase, expected int) {
		cases = append(cases, c)
		if c.ExitCode != expected {
			if c.ExitCode > worst {
				worst = c.ExitCode
			}
		}
	}
	add(successCase(opts, fixtures.clean), 0)
	add(lateCase(opts, fixtures.late), 0)
	add(truncatedCase(opts, fixtures.truncated), 2)
	add(beyondHorizonCase(opts, fixtures.clean), 3)
	add(resumeCase(opts, fixtures.clean), 0)
	add(interruptedCase(opts, fixtures.clean), 4)
	add(unsupportedCase(opts, fixtures.clean), 3)
	return cases, worst
}

// successCase checks the clean stream end to end.
func successCase(opts jobOptions, data []byte) demoCase {
	res, err := runPipeline(opts, data, timeline.DefaultConfig())
	if err != nil {
		return failureCase("success", err)
	}
	return summarizeCase("success", opts, res)
}

// lateCase feeds the late-arrival fixture through the watermark engine and
// proves the recompute moved the reported start earlier.
func lateCase(opts jobOptions, data []byte) demoCase {
	cfg := timeline.DefaultConfig()
	res, err := runPipeline(opts, data, cfg)
	if err != nil {
		return failureCase("late", err)
	}
	c := summarizeCase("late", opts, res)
	differing, shift, recomputedWindows, shiftErr := lateShift(data, opts.Target)
	if shiftErr != nil {
		c.ExitCode = media.ExitCode(shiftErr)
		c.Note = shiftErr.Error()
		return c
	}
	if recomputedWindows == 0 || differing == 0 {
		c.ExitCode = 1
		c.Note = "late fixture did not change the manifest windows"
		return c
	}
	c.Note = fmt.Sprintf("%d window(s) recomputed; %d manifest window(s) differ from strict (first gains %d event(s))",
		recomputedWindows, differing, shift)
	return c
}

// truncatedCase confirms a damaged fixture refuses to remux and reports the
// byte offset where the data ran out.
func truncatedCase(opts jobOptions, data []byte) demoCase {
	res, err := runPipeline(opts, data, timeline.DefaultConfig())
	c := demoCase{Name: "truncated", ExitCode: 2, ResumeOK: false}
	if err == nil {
		c.Note = "truncated fixture unexpectedly demuxed"
		return c
	}
	c.ExitCode = media.ExitCode(err)
	if off, ok := media.OffsetOf(err); ok {
		c.CorruptOff = off
	} else if res != nil && res.Input != nil {
		c.CorruptOff = res.Input.Bytes
	}
	c.Note = "truncation refused"
	return c
}

// resumeCase proves an interrupted write resumes to the same digest.
func resumeCase(opts jobOptions, reference []byte) demoCase {
	equal, recoverable, err := resumeRun(opts, reference)
	c := demoCase{Name: "resume", ResumeOK: equal}
	if err != nil {
		c.ExitCode = media.ExitCode(err)
		if c.ExitCode == 0 {
			c.ExitCode = 1
		}
		c.Note = err.Error()
		return c
	}
	if !equal {
		c.ExitCode = 1
		c.Note = "resumed digest differs"
		return c
	}
	if !recoverable {
		c.ExitCode = 1
		c.Note = "interruption not recoverable"
		return c
	}
	c.Note = "resumed digest equals full run; write failure is class 4"
	return c
}

// unsupportedCase demonstrates a target that cannot carry the input codecs.
func unsupportedCase(opts jobOptions, data []byte) demoCase {
	// Deliberately skip adaptForTarget: the raw AVC fixture must be rejected
	// by the WebM gateway before any byte is written.
	res, err := runUnadapted(opts, data, mapping.TargetWebM)
	c := demoCase{Name: "unsupported", ExitCode: 3}
	if err == nil {
		c.ExitCode = 0
		c.Note = "webm accepted AVC; gate missing"
		return c
	}
	c.ExitCode = media.ExitCode(err)
	if res != nil && res.Input != nil {
		c.CorruptOff = -1
	}
	c.Note = "mapping rejected before write"
	return c
}

// summarizeCase derives the reported metrics from a successful pipeline run.
func summarizeCase(name string, opts jobOptions, res *processResult) demoCase {
	actual := res.OutputBPS
	target := opts.TargetBPS
	if target <= 0 {
		target = res.InputBPS
	}
	delta := 0.0
	if target > 0 {
		delta = float64(actual-target) / float64(target) * 100
	}
	c := demoCase{
		Name:       name,
		TargetBPS:  target,
		ActualBPS:  actual,
		DeltaPct:   delta,
		Mismatch:   res.Verify.Mismatches,
		CRCErrors:  res.CRCBad,
		BufferPeak: res.BufferPeak,
		Replay:     digestHex(res.Replay),
		ResumeOK:   false,
		ExitCode:   0,
	}
	// Manifest skew: compare the manifest's declared range with the actual
	// first/last packet timestamps.
	var first, last media.Timestamp
	for i, p := range res.Packets {
		if i == 0 || p.DTS < first {
			first = p.DTS
		}
		if p.End() > last {
			last = p.End()
		}
	}
	skew := res.Summary.TimeStart - first
	if d := res.Summary.TimeEnd - last; d > skew || -d > skew {
		skew = d
	}
	if skew < 0 {
		skew = -skew
	}
	c.Skew = skew
	if res.Verify.CRCErrors != 0 {
		c.CRCErrors += res.Verify.CRCErrors
	}
	// Build and commit the manifest, and keep it for diagnostics.
	b := audit.NewBuilder(fmt.Sprintf("demo-%s-%d", name, opts.Seed), "generated", opts.Target)
	b.SetRate(target, actual)
	b.SetOutput(res.Summary)
	b.SetTimeline(res.Engine, res.Marked, res.Replay)
	b.SetBufferPeak(res.BufferPeak)
	b.SetVerification(res.Verify.Mismatches, c.CRCErrors)
	b.SetSkew(c.Skew)
	b.SetTracks(res.Plan, res.Packets)
	if err := b.Commit(); err != nil {
		c.ExitCode = media.ExitCode(err)
		c.Note = err.Error()
	}
	c.Manifest = b.Manifest()
	// Persist a manifest per case when a workdir is available, so a delivery
	// lead can inspect the recorded tracks, mapping, and timeline.
	if opts.WorkDir != "" {
		path := filepath.Join(opts.WorkDir, "manifests", name+".json")
		if err := b.WriteFile(path); err != nil && c.ExitCode == 0 {
			c.ExitCode = media.ExitCode(err)
			c.Note = err.Error()
		}
	}
	return c
}

func failureCase(name string, err error) demoCase {
	c := demoCase{Name: name, ExitCode: media.ExitCode(err), ResumeOK: false}
	if off, ok := media.OffsetOf(err); ok {
		c.CorruptOff = off
	}
	c.Note = err.Error()
	return c
}
