// Command avrepair is the terminal front-end for the synthetic media repair
// pipeline. It generates self-contained fixtures, runs the full clock/subtitle/
// track/index pipeline and emits deterministic quality summaries.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"avrepair/internal/engine"
	"avrepair/internal/fixture"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return engine.ExitArgs
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "repair":
		return runRepair(args[1:])
	case "-h", "--help", "help":
		usage()
		return engine.ExitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		usage()
		return engine.ExitArgs
	}
}

// demoConfig holds parsed demo flags.
type demoConfig struct {
	out   string
	cases []string
	rerun bool
	json  bool
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	cfg := demoConfig{}
	caseList := fs.String("cases", strings.Join(fixture.KnownCases, ","),
		"comma-separated scenarios to run")
	fs.StringVar(&cfg.out, "out", "", "output directory (required)")
	fs.BoolVar(&cfg.rerun, "rerun", false, "verify rerun determinism")
	fs.BoolVar(&cfg.json, "json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return engine.ExitArgs
	}
	if cfg.out == "" {
		fmt.Fprintln(os.Stderr, "demo: --out is required")
		return engine.ExitArgs
	}
	cfg.cases = splitCases(*caseList)
	if len(cfg.cases) == 0 {
		fmt.Fprintln(os.Stderr, "demo: no cases selected")
		return engine.ExitArgs
	}
	for _, name := range cfg.cases {
		if !knownCase(name) {
			fmt.Fprintf(os.Stderr, "demo: unknown case %q (valid: %s)\n",
				name, strings.Join(fixture.KnownCases, ","))
			return engine.ExitArgs
		}
	}

	var reports []*engine.Report
	qualityFailure := false
	// The batch harness returns 0 once every case has produced a report, with
	// per-case outcomes visible in the JSON. The one propagated hard failure is
	// exit code 4 (a genuine quality/index invariant violation); an
	// interrupted input (code 3) is expected simulated data, not a harness fault.
	for _, name := range cfg.cases {
		sc, err := fixture.Build(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "demo: build %s: %v\n", name, err)
			return engine.ExitUnrecoverable
		}
		rep, err := engine.Run(sc, engine.Options{
			OutDir: cfg.out, Rerun: cfg.rerun, EmitJSON: cfg.json,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "demo: %s: %v\n", name, err)
			return engine.ExitUnrecoverable
		}
		reports = append(reports, rep)
		if rep.ExitCode == engine.ExitQualityFailure {
			qualityFailure = true
		}
		if !cfg.json {
			printText(rep)
		}
	}
	if cfg.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			fmt.Fprintf(os.Stderr, "demo: encode: %v\n", err)
			return engine.ExitUnrecoverable
		}
	}
	if qualityFailure {
		return engine.ExitQualityFailure
	}
	return engine.ExitOK
}

// runRepair is the single-input entry point used by external callers; for the
// self-contained demo it maps one named case through the same pipeline.
func runRepair(args []string) int {
	fs := flag.NewFlagSet("repair", flag.ContinueOnError)
	out := fs.String("out", "", "output directory (required)")
	scenario := fs.String("case", fixture.CaseClean, "scenario to repair")
	rerun := fs.Bool("rerun", false, "verify rerun determinism")
	if err := fs.Parse(args); err != nil {
		return engine.ExitArgs
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "repair: --out is required")
		return engine.ExitArgs
	}
	if !knownCase(*scenario) {
		fmt.Fprintf(os.Stderr, "repair: unknown case %q\n", *scenario)
		return engine.ExitArgs
	}
	sc, err := fixture.Build(*scenario)
	if err != nil {
		fmt.Fprintf(os.Stderr, "repair: %v\n", err)
		return engine.ExitUnrecoverable
	}
	rep, err := engine.Run(sc, engine.Options{OutDir: *out, Rerun: *rerun})
	if err != nil {
		fmt.Fprintf(os.Stderr, "repair: %v\n", err)
		return engine.ExitUnrecoverable
	}
	printText(rep)
	return rep.ExitCode
}

func printText(r *engine.Report) {
	fmt.Printf("case=%s target_bps=%d actual_bps=%d drift_pct=%.4f "+
		"audio_rms_delta_db=%.4f video_luma_mae=%.4f manifest_max_skew_ms=%.4f "+
		"repair_count=%d seek_miss=%d isolated=%d exit=%d\n  sha256=%s\n",
		r.Case, r.TargetBPS, r.ActualBPS, r.DriftPct,
		r.AudioRMSDeltaDB, r.VideoLumaMAE, r.ManifestMaxSkewMS,
		r.RepairCount, r.SeekMiss, r.Isolated, r.ExitCode, r.OutputSHA256)
}

func splitCases(list string) []string {
	var out []string
	for _, c := range strings.Split(list, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

func knownCase(name string) bool {
	for _, k := range fixture.KnownCases {
		if k == name {
			return true
		}
	}
	return false
}

func usage() {
	fmt.Fprint(os.Stderr, `avrepair - synthetic media repair pipeline

usage:
  avrepair demo   --out DIR [--cases a,b,...] [--rerun] [--json]
  avrepair repair --out DIR [--case clean|rollback|overlap|meta-conflict|truncated|interrupted] [--rerun]

exit codes:
  0 completed   2 argument error   3 unrecoverable input   4 quality/index failure
`)
}
