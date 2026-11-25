package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"fieldqc/internal/report"
	"fieldqc/internal/stream"
)

func init() {
	registerCommand("demo", "synthesize and analyze QC scenarios", runDemo)
}

// runDemo implements `fieldqc demo ...`. Exit 0 when --case all meets
// every expectation; a single damaged case that locates a span exits 2.
func runDemo(argv []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	caseName := fs.String("case", "all", "which scenario to run")
	seed := fs.Int64("seed", 41, "deterministic sample seed")
	hours := fs.Int("hours", 2, "media duration per asset, in hours")
	tracks := fs.Int("tracks", 4, "tracks per asset (half video, half audio)")
	loss := fs.Float64("loss", 0.03, "random RTP loss probability")
	window := fs.Duration("window", 10*time.Second, "media window per pipeline item")
	if err := fs.Parse(argv); err != nil {
		return exitInvalid
	}
	if *hours <= 0 || *tracks < 2 || *tracks%2 != 0 || *loss < 0 || *loss >= 1 || *window <= 0 {
		fmt.Fprintln(os.Stderr, "fieldqc: invalid parameters (need hours>0, even tracks>=2, 0<=loss<1, window>0)")
		return exitInvalid
	}
	cases := allCases
	if *caseName != "all" {
		if !slices.Contains(allCases, *caseName) {
			fmt.Fprintf(os.Stderr, "fieldqc: unknown case %q (want all|%s)\n", *caseName, joinCases())
			return exitInvalid
		}
		cases = []string{*caseName}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := config{seed: *seed, hours: *hours, tracks: *tracks, loss: *loss, window: *window}

	diverged := false
	for _, c := range cases {
		line, err := runCase(ctx, cfg, c)
		if errors.Is(err, stream.ErrAborted) || ctx.Err() != nil {
			_ = report.WriteLine(os.Stdout, report.Line{Case: c, Status: "aborted"})
			return exitAborted
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "fieldqc: case %s: %v\n", c, err)
			return exitAborted
		}
		// Damaged scenarios must locate at least one span; the clean
		// scenario must locate none.
		if wantDamage := c != "normal"; (line.DamagedSpans > 0) != wantDamage {
			diverged = true
		}
		if err := report.WriteLine(os.Stdout, line); err != nil {
			fmt.Fprintf(os.Stderr, "fieldqc: write report: %v\n", err)
			return exitAborted
		}
	}
	if diverged {
		return exitDamaged // an expected span was missing or clean media was flagged
	}
	if len(cases) == 1 && cases[0] != "normal" {
		return exitDamaged // input corruption located
	}
	return exitOK
}
