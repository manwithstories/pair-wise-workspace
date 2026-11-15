package main

import (
	"errors"
	"fmt"
	"io"

	"rcctl/internal/plan"
	"rcctl/internal/report"
	"rcctl/internal/state"
)

func commandVerify(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("verify", stderr)
	reportPath := fs.String("report", "work/report.json", "report input path")
	maxDev := fs.Float64("max-dev-pct", 5, "maximum absolute bitrate deviation percent")
	maxStd := fs.Float64("max-psnr-std", 1.5, "maximum PSNR standard deviation in dB")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	if *maxDev < 0 || *maxStd < 0 {
		return failInvalid(stderr, fmt.Errorf("verification thresholds must be non-negative"))
	}
	rep, err := report.Load(*reportPath)
	if err != nil {
		return failInvalid(stderr, fmt.Errorf("load report: %w", err))
	}
	report.Print(stdout, rep)
	violations := report.Verify(rep, *maxDev, *maxStd)
	if len(violations) == 0 {
		fmt.Fprintf(stdout, "verify: PASS (max_dev_pct<=%.2f, psnr_std_db<=%.2f)\n", *maxDev, *maxStd)
		return exitOK
	}
	fmt.Fprintf(stdout, "verify: FAIL (%d violation(s))\n", len(violations))
	for _, v := range violations {
		if v.Index < 0 {
			fmt.Fprintf(stdout, "  overall: %s\n", v.Reason)
		} else {
			fmt.Fprintf(stdout, "  segment %d: %s\n", v.Index, v.Reason)
		}
	}
	return exitThreshold
}

func commandReport(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("report", stderr)
	planPath := fs.String("plan", "work/plan.json", "plan input path")
	journalPath := fs.String("journal", "work/journal.log", "journal input path")
	out := fs.String("out", "", "report output path (default: beside journal)")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	p, err := plan.Load(*planPath)
	if err != nil {
		return failInvalid(stderr, fmt.Errorf("load plan: %w", err))
	}
	snapshot, err := state.Read(*journalPath)
	if err != nil {
		fmt.Fprintf(stderr, "rcctl: read journal: %v\n", err)
		return exitRuntime
	}
	for _, warning := range snapshot.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", warning)
	}
	if err := validateJournal(snapshot, p); err != nil {
		if errors.Is(err, state.ErrPlanMismatch) {
			fmt.Fprintf(stderr, "rcctl: journal/plan mismatch: %v\n", err)
			return exitJournal
		}
		return failInvalid(stderr, err)
	}
	rep := report.Build(p, snapshot.CompletedList(), 0)
	path := resolveReportPath(*out, *journalPath)
	if err := report.Save(path, rep); err != nil {
		fmt.Fprintf(stderr, "rcctl: write report: %v\n", err)
		return exitRuntime
	}
	report.Print(stdout, rep)
	fmt.Fprintf(stdout, "wrote %s\n", path)
	if rep.Summary.Completed < rep.Summary.Segments {
		fmt.Fprintf(stderr, "rcctl: report contains %d incomplete segment(s)\n",
			rep.Summary.Segments-rep.Summary.Completed)
		return exitThreshold
	}
	return exitOK
}
