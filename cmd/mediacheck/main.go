// Command mediacheck is the terminal acceptance entry point. It generates
// fixtures and analyzes MP4/MOV inputs, printing a machine-readable report and
// returning 0 (complete), 2 (damaged but recovered) or 1 (argument or
// unrecoverable failure).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"mediacheck/internal/fanout"
	"mediacheck/internal/fault"
	"mediacheck/internal/fixture"
	"mediacheck/internal/probe"
	"mediacheck/internal/repair"
	"mediacheck/internal/report"
)

const usage = `mediacheck - inspect MP4/MOV structure, recover damaged samples, verify bitrate ladders

Usage:
  mediacheck generate --dir DIR
  mediacheck analyze --input FILE [--ladder 300000,600000] [--segment 2s] [--recover]
                     [--tolerance 1ms] [--json] [--no-fill-detect]

Exit codes: 0 complete, 2 damaged but recovered, 1 argument or unrecoverable error.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return report.ExitFailure
	}
	switch args[0] {
	case "generate":
		return runGenerate(args[1:], stdout, stderr)
	case "analyze":
		return runAnalyze(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return report.ExitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return report.ExitFailure
	}
}

func runGenerate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "directory to write fixtures into (required)")
	if err := fs.Parse(args); err != nil {
		return report.ExitFailure
	}
	if *dir == "" {
		fmt.Fprintln(stderr, "generate: --dir is required")
		return report.ExitFailure
	}
	results, err := fixture.GenerateAll(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "generate: %v\n", err)
		return report.ExitFailure
	}
	for _, r := range results {
		fmt.Fprintf(stdout, "%-18s %8d bytes  sha256=%s\n", r.Kind, r.Bytes, r.SHA256)
	}
	return report.ExitOK
}

type options struct {
	input     string
	ladder    string
	segment   string
	tolerance string
	recover   bool
	jsonOut   bool
	noFill    bool
	maxMoov   int64
}

func runAnalyze(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(&o.input, "input", "", "input MP4/MOV file (required)")
	fs.StringVar(&o.ladder, "ladder", "300000,600000", "comma-separated target bitrates in bps")
	fs.StringVar(&o.segment, "segment", "2s", "target segment duration (e.g. 2s, 500ms, 2000)")
	fs.StringVar(&o.tolerance, "tolerance", "1ms", "cross-layer boundary tolerance")
	fs.BoolVar(&o.recover, "recover", false, "enable sample recovery")
	fs.BoolVar(&o.jsonOut, "json", false, "emit JSON instead of the text summary")
	fs.BoolVar(&o.noFill, "no-fill-detect", false, "disable uniform-fill corruption detection")
	fs.Int64Var(&o.maxMoov, "max-moov", 0, "cap the in-memory moov payload in bytes")
	if err := fs.Parse(args); err != nil {
		return report.ExitFailure
	}
	if o.input == "" {
		fmt.Fprintln(stderr, "analyze: --input is required")
		return report.ExitFailure
	}

	ladder, err := parseLadder(o.ladder)
	if err != nil {
		fmt.Fprintf(stderr, "analyze: %v\n", err)
		return report.ExitFailure
	}
	segMS, err := parseDuration(o.segment)
	if err != nil {
		fmt.Fprintf(stderr, "analyze: --segment: %v\n", err)
		return report.ExitFailure
	}
	tolMS, err := parseDuration(o.tolerance)
	if err != nil {
		fmt.Fprintf(stderr, "analyze: --tolerance: %v\n", err)
		return report.ExitFailure
	}

	probeCfg := probe.DefaultConfig()
	probeCfg.SegmentMS = segMS
	if o.maxMoov > 0 {
		probeCfg.MaxMoovBytes = o.maxMoov
	}
	repCfg := repair.DefaultConfig()
	repCfg.SegmentMS = segMS
	repCfg.ToleranceMS = tolMS
	repCfg.Recover = o.recover
	repCfg.DetectUniformFill = !o.noFill
	fanCfg := fanout.Config{LadderBPS: ladder, SegmentMS: segMS, ToleranceMS: tolMS}

	pf, res, err := repair.New(repCfg, probeCfg).Analyze(o.input)
	if err != nil {
		f := faultSummary(err)
		if o.jsonOut {
			_ = writeFailureJSON(stdout, o.input, f)
		} else {
			fmt.Fprintf(stderr, "analyze: %s\n", f.Detail)
		}
		return report.ExitFailure
	}
	var man *fanout.Manifest
	if m, mErr := fanout.FromRecovery(fanout.ManifestInput{Recovery: res, Config: fanCfg}); mErr == nil {
		man = m
	} else if o.recover {
		fmt.Fprintf(stderr, "analyze: manifest: %v\n", mErr)
	}

	exit := report.ExitFor(res)
	summary := report.Build(pf, res, man, exit)
	if o.jsonOut {
		if err := summary.Marshal(stdout); err != nil {
			fmt.Fprintf(stderr, "analyze: write report: %v\n", err)
			return report.ExitFailure
		}
	} else {
		if err := summary.Text(stdout); err != nil {
			fmt.Fprintf(stderr, "analyze: write report: %v\n", err)
			return report.ExitFailure
		}
	}
	if f, ok := fault.As(err); ok && f.Code == fault.CodeInvalidArgument {
		return report.ExitFailure
	}
	return exit
}

func parseLadder(s string) ([]int64, error) {
	parts := strings.Split(s, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid bitrate %q", p)
		}
		if v <= 0 {
			return nil, fmt.Errorf("bitrate must be positive: %q", p)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, errors.New("ladder is empty")
	}
	return out, nil
}

// parseDuration accepts Go durations (2s, 500ms) and bare millisecond counts.
func parseDuration(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty duration")
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		if v <= 0 {
			return 0, fmt.Errorf("duration must be positive: %q", s)
		}
		return v, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	ms := d.Milliseconds()
	if ms <= 0 {
		return 0, fmt.Errorf("duration must be positive: %q", s)
	}
	return ms, nil
}

func faultSummary(err error) report.IssueSummary {
	if f, ok := fault.As(err); ok {
		return report.IssueSummary{Code: f.Code, Severity: string(f.Severity), Op: f.Op, Offset: f.Offset, Detail: f.Error()}
	}
	return report.IssueSummary{Code: fault.CodeIO, Severity: string(fault.SeverityFatal), Offset: -1, Detail: err.Error()}
}

func writeFailureJSON(w io.Writer, path string, is report.IssueSummary) error {
	s := &report.Summary{
		Format:       "mediacheck/v1",
		Path:         path,
		Status:       "failed",
		ExitCode:     report.ExitFailure,
		DamageOffset: -1,
		Issues:       []report.IssueSummary{is},
	}
	return s.Marshal(w)
}
