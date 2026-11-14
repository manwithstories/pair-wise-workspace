package main

import (
	"fmt"
	"os"

	"subtool/internal/fixtures"
	"subtool/internal/report"
	"subtool/internal/timebase"
)

func runFixtures(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 || args[0] != "gen" {
		fmt.Fprintln(stderr, "usage: subtool fixtures gen --out DIR --count N --seed N")
		return statusArgs
	}
	flags := newFlagSet("fixtures gen", stderr)
	out := flags.String("out", "", "output directory")
	count := flags.Int("count", 1000, "cue count")
	seed := flags.Int64("seed", 7, "deterministic seed")
	if err := flags.Parse(args[1:]); err != nil {
		return statusArgs
	}
	if *out == "" || *count <= 0 {
		fmt.Fprintln(stderr, "--out and a positive --count are required")
		return statusArgs
	}
	summary, err := fixtures.Generate(*out, *count, *seed)
	if err != nil {
		return emitFailure(stderr, "", &failure{code: "fixture_generation_failed", message: err.Error(), status: statusInput})
	}
	fmt.Fprintf(stdout, "generated %d files in %s (cues=%d overlap_groups=%d)\n",
		summary.Files, summary.OutDir, summary.Cues, summary.OverlapGroups)
	return statusOK
}

func runProbe(args []string, stdout, stderr *os.File) int {
	flags := newFlagSet("probe", stderr)
	in := flags.String("in", "", "input subtitle")
	jsonPath := flags.String("json", "", "JSON report path")
	if err := flags.Parse(args); err != nil {
		return statusArgs
	}
	if *in == "" {
		fmt.Fprintln(stderr, "--in is required")
		return statusArgs
	}
	_, enc, fail := loadSource(*in)
	if fail != nil {
		return emitFailure(stderr, *jsonPath, fail)
	}
	track, fail := parseDecoded(*in, enc)
	if fail != nil {
		return emitFailure(stderr, *jsonPath, fail)
	}
	enc.Text = nil
	value := report.BuildProbe(*in, track.Format, enc, track)
	value.Warnings = lowConfidenceWarnings(enc)
	if err := report.WriteJSON(*jsonPath, value); *jsonPath != "" && err != nil {
		return emitFailure(stderr, "", &failure{code: "report_write_failed", message: err.Error(), status: statusInput})
	}
	report.PrintProbe(stdout, value)
	return encodingStatus(enc, false)
}

func runFrame(args []string, stdout, stderr *os.File) int {
	flags := newFlagSet("frame", stderr)
	in := flags.String("in", "", "input subtitle")
	fps := flags.String("fps", "", "frame rate NUM/DEN")
	jsonPath := flags.String("json", "", "JSON report path")
	if err := flags.Parse(args); err != nil {
		return statusArgs
	}
	if *in == "" || *fps == "" {
		fmt.Fprintln(stderr, "--in and --fps are required")
		return statusArgs
	}
	rate, err := timebase.ParseRate(*fps)
	if err != nil {
		return emitFailure(stderr, *jsonPath, &failure{code: "invalid_fps", message: err.Error(), status: statusArgs})
	}
	_, enc, fail := loadSource(*in)
	if fail != nil {
		return emitFailure(stderr, *jsonPath, fail)
	}
	track, fail := parseDecoded(*in, enc)
	if fail != nil {
		return emitFailure(stderr, *jsonPath, fail)
	}
	enc.Text = nil
	value := report.BuildFrames(*in, rate, track)
	value.Encoding = enc
	value.Warnings = lowConfidenceWarnings(enc)
	if err := report.WriteJSON(*jsonPath, value); *jsonPath != "" && err != nil {
		return emitFailure(stderr, "", &failure{code: "report_write_failed", message: err.Error(), status: statusInput})
	}
	report.PrintFrames(stdout, value)
	return encodingStatus(enc, false)
}
