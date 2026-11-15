package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"rcctl/internal/plan"
	"rcctl/internal/report"
	"rcctl/internal/runner"
	"rcctl/internal/state"
)

func commandRun(args []string, stdout, stderr io.Writer, resume bool) int {
	name := "run"
	if resume {
		name = "resume"
	}
	fs := newFlagSet(name, stderr)
	planPath := fs.String("plan", "work/plan.json", "plan input path")
	workers := fs.Int("workers", 2, "concurrent segment workers")
	journalPath := fs.String("journal", "work/journal.log", "append-only journal path")
	reportPath := fs.String("report", "", "report output path (default: beside journal)")
	retries := fs.Int("retries", 3, "total attempts per segment")
	backoffMS := fs.Int("backoff-ms", 100, "initial retry backoff in milliseconds")
	passes := fs.Int("rate-passes", 6, "maximum rate-control encode passes")
	tolerance := fs.Float64("rate-tolerance-pct", 0.5, "acceptable target deviation per encode pass")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	if *workers < 1 || *retries < 1 || *backoffMS < 0 || *passes < 1 || *tolerance < 0 {
		return failInvalid(stderr, fmt.Errorf("workers, retries, and rate-passes must be positive"))
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
	resumed := len(snapshot.Completed)
	if resume && resumed == 0 {
		fmt.Fprintln(stderr, "warning: no completed segments found; resume will encode all segments")
	}
	journal, err := state.Open(*journalPath)
	if err != nil {
		fmt.Fprintf(stderr, "rcctl: open journal: %v\n", err)
		return exitRuntime
	}
	defer journal.Close()
	if err := journal.Append(state.Record{Type: state.RunStarted, PlanID: p.PlanID,
		Segment: -1, Message: name}); err != nil {
		fmt.Fprintf(stderr, "rcctl: write journal: %v\n", err)
		return exitRuntime
	}
	var logMu sync.Mutex
	logf := func(format string, values ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(stderr, format+"\n", values...)
	}
	results, failed, err := runner.Execute(context.Background(), p, snapshot.Completed, journal, runner.Options{
		Workers: *workers, MaxAttempts: *retries, Backoff: time.Duration(*backoffMS) * time.Millisecond,
		RatePasses: *passes, RateTolerance: *tolerance, Logf: logf,
	})
	if err != nil {
		fmt.Fprintf(stderr, "rcctl: execution failed: %v\n", err)
		return exitRuntime
	}
	rep := report.Build(p, results, resumed)
	resolvedReport := resolveReportPath(*reportPath, *journalPath)
	if err := report.Save(resolvedReport, rep); err != nil {
		fmt.Fprintf(stderr, "rcctl: write report: %v\n", err)
		return exitRuntime
	}
	report.Print(stdout, rep)
	fmt.Fprintf(stdout, "wrote %s\n", resolvedReport)
	if len(failed) > 0 {
		fmt.Fprintf(stderr, "rcctl: %d segment(s) exhausted retries and remain failed\n", len(failed))
		return exitThreshold
	}
	return exitOK
}
