package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mediac/internal/fixture"
	"mediac/internal/report"
)

// Exit codes follow the acceptance contract.
const (
	exitOK      = 0 // samples passed, no damage
	exitConfig  = 1 // input/configuration error
	exitDamage  = 2 // processing completed, media defects found
	exitBlocked = 3 // retries exhausted with a blocked node
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: mediac demo --out DIR [--cases a,b] [--workers N] [--retry N]")
		return exitConfig
	}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	out := fs.String("out", "", "output directory")
	casesFlag := fs.String("cases", strings.Join(fixture.AllCases, ","),
		"comma-separated fixture cases")
	workers := fs.Int("workers", 4, "DAG worker concurrency")
	retries := fs.Int("retry", 2, "max retries per node")
	if err := fs.Parse(args[1:]); err != nil {
		return exitConfig
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "demo: --out is required")
		return exitConfig
	}
	if *workers <= 0 || *retries < 0 {
		fmt.Fprintln(os.Stderr, "demo: workers must be > 0 and retry >= 0")
		return exitConfig
	}
	cases, err := resolveCases(*casesFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
		return exitConfig
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "demo: cannot create out dir: %v\n", err)
		return exitConfig
	}

	p := &pipeline{outDir: *out, workers: *workers, retries: *retries}
	reportPath := filepath.Join(*out, "report.jsonl")
	writer, err := report.NewWriter(reportPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: report: %v\n", err)
		return exitConfig
	}

	anyDamage, anyBlocked, anyConfigErr := false, false, false
	for _, name := range cases {
		res, err := p.runCase(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "demo: case %s: %v\n", name, err)
			anyConfigErr = true
			continue
		}
		stored, err := writer.Append(res.record)
		if err != nil {
			fmt.Fprintf(os.Stderr, "demo: write record: %v\n", err)
			anyConfigErr = true
			continue
		}
		anyDamage = anyDamage || res.hasDamage
		anyBlocked = anyBlocked || res.hasBlocked
		printRecord(stored)
	}
	if err := writer.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "demo: close report: %v\n", err)
		return exitConfig
	}
	fmt.Printf("report: %s (content_sha256=%s)\n", reportPath, writer.ContentHash())

	switch {
	case anyConfigErr:
		return exitConfig
	case anyBlocked:
		return exitBlocked
	case anyDamage:
		return exitDamage
	default:
		return exitOK
	}
}

func resolveCases(flagValue string) ([]string, error) {
	var out []string
	for _, name := range strings.Split(flagValue, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !knownCase(name) {
			return nil, fmt.Errorf("unknown case %q (valid: %s)",
				name, strings.Join(fixture.AllCases, ","))
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no cases selected")
	}
	return out, nil
}

func knownCase(name string) bool {
	for _, c := range fixture.AllCases {
		if c == name {
			return true
		}
	}
	return false
}

func printRecord(r report.Record) {
	fmt.Printf("case=%s reps=%d segments=%d target_bps=%d actual_bps=%d "+
		"delta=%.2f%% valid_sample=%.2f%% gop_p95=%.0fms damaged=%d "+
		"align_max=%dms retry_reused=%d first_blocked=%q sha256=%s\n",
		r.Case, r.Representations, r.Segments, r.TargetBPS, r.ActualBPS,
		r.BitrateDeltaPct, r.ValidSamplePct, r.GOPP95Ms, len(r.DamagedRanges),
		r.AlignmentMaxMs, r.RetryReused, r.FirstBlocked, r.ReportSHA256)
}
