package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"loudness/internal/batch"
	"loudness/internal/gen"
	"loudness/internal/loudness"
	"loudness/internal/report"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 3
	}
	switch args[0] {
	case "measure":
		return runMeasure(args[1:], stdout, stderr)
	case "batch":
		return runBatch(args[1:], stdout, stderr)
	case "selftest":
		return runSelftest(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		usage(stderr)
		return 3
	}
}

func runMeasure(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("measure", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("in", "", "input WAV file")
	target := fs.Float64("target", -23, "target integrated loudness (LUFS)")
	ceiling := fs.Float64("ceiling", -1, "true-peak ceiling (dBTP)")
	asJSON := fs.Bool("json", false, "emit one JSON line")
	outPath := fs.String("o", "", "write output to file")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *input == "" || !validLevels(*target, *ceiling) {
		usage(stderr)
		return 3
	}
	m, err := loudness.NewProcessor().ProcessFile(*input)
	if err != nil {
		fmt.Fprintf(stderr, "measure: %v\n", err)
		return 2
	}
	row := report.Judge(filepath.Base(*input), m, *target, *ceiling, report.DefaultToleranceLU)
	if err := renderTo(*outPath, stdout, func(w io.Writer) error {
		return report.RenderMeasure(w, row, *asJSON)
	}); err != nil {
		fmt.Fprintf(stderr, "measure: output: %v\n", err)
		return 3
	}
	if row.Verdict != report.VerdictPass {
		return 1
	}
	return 0
}

func runBatch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("batch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "directory containing WAV files")
	jobs := fs.Int("jobs", 4, "worker count")
	target := fs.Float64("target", -23, "target integrated loudness (LUFS)")
	ceiling := fs.Float64("ceiling", -1, "true-peak ceiling (dBTP)")
	asJSON := fs.Bool("json", false, "emit one JSON document")
	outPath := fs.String("o", "", "write output to file")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *dir == "" || *jobs <= 0 || !validLevels(*target, *ceiling) {
		usage(stderr)
		return 3
	}
	result, err := batch.Run(batch.Options{
		Dir: *dir, Jobs: *jobs, Target: *target, Ceiling: *ceiling,
		Tolerance: report.DefaultToleranceLU,
	})
	if err != nil {
		fmt.Fprintf(stderr, "batch: %v\n", err)
		return 2
	}
	if err := renderTo(*outPath, stdout, func(w io.Writer) error {
		return report.RenderBatch(w, result.Rows, result.Summary, *asJSON)
	}); err != nil {
		fmt.Fprintf(stderr, "batch: output: %v\n", err)
		return 3
	}
	if result.Summary.Error > 0 {
		return 2
	}
	if result.Summary.Fail > 0 {
		return 1
	}
	return 0
}

func runSelftest(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		usage(stderr)
		return 3
	}
	dir, err := os.MkdirTemp("", "loudness-selftest-*")
	if err != nil {
		fmt.Fprintf(stderr, "selftest: %v\n", err)
		return 2
	}
	defer os.RemoveAll(dir)
	cases, err := gen.GenerateSelftest(dir)
	if err != nil {
		fmt.Fprintf(stderr, "selftest: generate: %v\n", err)
		return 2
	}
	processor := loudness.NewProcessor()
	failed := false
	for _, tc := range cases {
		m, err := processor.ProcessFile(tc.Path)
		if err != nil {
			fmt.Fprintf(stderr, "selftest: %s: %v\n", tc.Name, err)
			return 2
		}
		errValue := 0.0
		if tc.ExpectedReason != "" {
			if m.Reason != tc.ExpectedReason {
				errValue = 1
				failed = true
			}
			fmt.Fprintf(stdout, "%s reason expected=%s actual=%s err=%.2f\n",
				tc.Name, tc.ExpectedReason, reasonOrNone(m.Reason), errValue)
			continue
		}
		if tc.CheckIntegrated {
			errValue = math.Max(errValue, math.Abs(m.IntegratedLUFS-tc.ExpectedIntegrated))
			if errValue > tc.Tolerance {
				failed = true
			}
		}
		if tc.CheckTruePeak {
			errValue = math.Max(errValue, math.Abs(m.TruePeakDBTP-tc.ExpectedTruePeak))
			if errValue > tc.Tolerance {
				failed = true
			}
		}
		switch {
		case tc.CheckIntegrated && tc.CheckTruePeak:
			fmt.Fprintf(stdout, "%s integrated expected=%.2f actual=%.2f true_peak expected=%+.2f actual=%+.2f err=%.2f\n",
				tc.Name, tc.ExpectedIntegrated, m.IntegratedLUFS, tc.ExpectedTruePeak, m.TruePeakDBTP, errValue)
		case tc.CheckIntegrated:
			fmt.Fprintf(stdout, "%s integrated expected=%.2f actual=%.2f err=%.2f\n",
				tc.Name, tc.ExpectedIntegrated, m.IntegratedLUFS, errValue)
		case tc.CheckTruePeak:
			fmt.Fprintf(stdout, "%s true_peak expected=%+.2f actual=%+.2f err=%.2f\n",
				tc.Name, tc.ExpectedTruePeak, m.TruePeakDBTP, errValue)
		}
	}
	if failed {
		return 1
	}
	return 0
}

func renderTo(path string, stdout io.Writer, render func(io.Writer) error) error {
	if path == "" {
		return render(stdout)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	renderErr := render(f)
	closeErr := f.Close()
	return errors.Join(renderErr, closeErr)
}

func reasonOrNone(reason string) string {
	if reason == "" {
		return "none"
	}
	return reason
}

func validLevels(target, ceiling float64) bool {
	return !math.IsNaN(target) && !math.IsInf(target, 0) && target <= 0 &&
		!math.IsNaN(ceiling) && !math.IsInf(ceiling, 0) && ceiling <= 0
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  loudness measure --in FILE [--target -23] [--ceiling -1] [--json] [-o FILE]")
	fmt.Fprintln(w, "  loudness batch --dir DIR [--jobs 4] [--target -23] [--ceiling -1] [--json] [-o FILE]")
	fmt.Fprintln(w, "  loudness selftest")
}
