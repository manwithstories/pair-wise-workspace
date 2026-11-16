// loudcheck is an offline loudness compliance checker: K-weighted,
// gated integrated loudness (LUFS), short-term loudness max and
// oversampled true peak for WAV files, single or in batch.
//
// Exit codes: 0 = all files pass, 1 = threshold failures present,
// 2 = input/parameter error, 3 = processing exception.
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"loudcheck/internal/audio"
	"loudcheck/internal/batch"
	"loudcheck/internal/meter"
	"loudcheck/internal/report"
	"loudcheck/testdata"
)

const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
	exitError = 3
)

// exitError carries a process exit code alongside the cause.
type codeError struct {
	code int
	err  error
}

func (e *codeError) Error() string { return e.err.Error() }
func (e *codeError) Unwrap() error { return e.err }

func usageErr(format string, args ...any) *codeError {
	return &codeError{exitUsage, fmt.Errorf(format, args...)}
}

type flags struct {
	input     []string
	layout    string
	shortWin  time.Duration
	peakFact  int
	target    float64
	tpLimit   float64
	workers   int
	jsonOut   string
	outDir    string
	seed      int64
	channels  int
}

func main() { os.Exit(run()) }

func run() int {
	f := &flags{}
	root := &cobra.Command{
		Use:          "loudcheck",
		Short:        "Offline loudness compliance checker (BS.1770-style)",
		SilenceUsage: true,
		SilenceErrors: true,
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &codeError{exitUsage, err}
	})

	measure := &cobra.Command{
		Use:   "measure",
		Short: "Measure a single WAV file",
		RunE:  func(_ *cobra.Command, _ []string) error { return doMeasure(f) },
	}
	batchCmd := &cobra.Command{
		Use:   "batch",
		Short: "Measure files or directories concurrently",
		RunE:  func(_ *cobra.Command, _ []string) error { return doBatch(f) },
	}
	fixture := &cobra.Command{
		Use:   "fixture",
		Short: "Generate deterministic test WAV fixtures",
		RunE:  func(_ *cobra.Command, _ []string) error { return doFixture(f) },
	}

	for _, c := range []*cobra.Command{measure, batchCmd} {
		c.Flags().StringSliceVarP(&f.input, "input", "i", nil, "WAV file(s) or directory(ies)")
		c.Flags().StringVar(&f.layout, "layout", "auto", "channel layout: auto|mono|stereo|4.0|5.1|7.1")
		c.Flags().DurationVar(&f.shortWin, "short-window", 3*time.Second, "short-term window length")
		c.Flags().IntVar(&f.peakFact, "peak-factor", 4, "true-peak oversampling factor: 4 or 8")
		c.Flags().Float64Var(&f.target, "target", -23.0, "integrated loudness ceiling (LUFS)")
		c.Flags().Float64Var(&f.tpLimit, "true-peak-limit", -1.0, "true peak ceiling (dBTP)")
		c.Flags().StringVar(&f.jsonOut, "json", "", "also write JSON report to this path")
	}
	batchCmd.Flags().IntVarP(&f.workers, "workers", "w", 4, "max concurrent measurements")
	fixture.Flags().StringVar(&f.outDir, "out", "", "output directory for fixtures")
	fixture.Flags().Int64Var(&f.seed, "seed", 1, "deterministic content seed")
	fixture.Flags().IntVar(&f.channels, "channels", 6, "channel count of the multichannel fixture (3..8)")

	root.AddCommand(measure, batchCmd, fixture)
	if err := root.Execute(); err != nil {
		var ee *codeError
		if errors.As(err, &ee) {
			fmt.Fprintln(os.Stderr, "error:", ee.err)
			return ee.code
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitError
	}
	return exitOK
}

// meterOptions validates flags and builds meter.Options.
func (f *flags) meterOptions() (meter.Options, error) {
	layout, err := audio.ParseLayout(f.layout)
	if err != nil {
		return meter.Options{}, usageErr("%v", err)
	}
	if f.peakFact != 4 && f.peakFact != 8 {
		return meter.Options{}, usageErr("--peak-factor must be 4 or 8, got %d", f.peakFact)
	}
	if f.shortWin < 500*time.Millisecond {
		return meter.Options{}, usageErr("--short-window must be >= 500ms, got %s", f.shortWin)
	}
	return meter.Options{
		Layout:        layout,
		ShortWindow:   f.shortWin,
		PeakFactor:    f.peakFact,
		TargetLUFS:    f.target,
		TruePeakLimit: f.tpLimit,
	}, nil
}

func (f *flags) requireInput() error {
	if len(f.input) == 0 {
		return usageErr("--input is required")
	}
	return nil
}

// emit writes the table to stdout and the JSON report if requested.
func (f *flags) emit(doc report.Document) error {
	report.WriteTable(os.Stdout, doc)
	if f.jsonOut != "" {
		out, err := os.Create(f.jsonOut)
		if err != nil {
			return &codeError{exitError, fmt.Errorf("write JSON report: %w", err)}
		}
		defer out.Close()
		if err := report.WriteJSON(out, doc); err != nil {
			return &codeError{exitError, fmt.Errorf("write JSON report: %w", err)}
		}
	}
	return nil
}

// exitFor maps a report summary to a process exit code.
func exitFor(s report.Summary) int {
	switch {
	case s.Errors > 0:
		return exitError
	case s.Failed > 0:
		return exitFail
	default:
		return exitOK
	}
}

func doMeasure(f *flags) error {
	if err := f.requireInput(); err != nil {
		return err
	}
	if len(f.input) != 1 {
		return usageErr("measure takes exactly one --input file")
	}
	opts, err := f.meterOptions()
	if err != nil {
		return err
	}
	res, err := meter.MeasureFile(f.input[0], opts)
	if err != nil {
		return &codeError{exitError, err}
	}
	doc := report.Build([]*meter.Result{res}, nil)
	if err := f.emit(doc); err != nil {
		return err
	}
	if code := exitFor(doc.Summary); code != exitOK {
		return &codeError{code, errors.New("threshold check failed")}
	}
	return nil
}

func doBatch(f *flags) error {
	if err := f.requireInput(); err != nil {
		return err
	}
	opts, err := f.meterOptions()
	if err != nil {
		return err
	}
	files, err := batch.Collect(f.input)
	if err != nil {
		return usageErr("%v", err)
	}
	out := batch.Run(files, opts, f.workers)
	doc := report.Build(out.Results, out.Errors)
	if err := f.emit(doc); err != nil {
		return err
	}
	if code := exitFor(doc.Summary); code != exitOK {
		return &codeError{code, errors.New("batch did not fully pass")}
	}
	return nil
}

func doFixture(f *flags) error {
	if f.outDir == "" {
		return usageErr("--out is required")
	}
	paths, err := fixture.Generate(fixture.Options{
		OutDir:   f.outDir,
		Seed:     f.seed,
		Channels: f.channels,
	})
	if err != nil {
		return &codeError{exitError, err}
	}
	for _, p := range paths {
		fmt.Println("wrote", p)
	}
	return nil
}
