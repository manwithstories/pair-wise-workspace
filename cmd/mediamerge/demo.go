package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"mediamerge/internal/demux"
	"mediamerge/internal/fixture"
	"mediamerge/internal/mapping"
	"mediamerge/internal/txn"
	"mediamerge/internal/verify"
)

// runDemo generates a deterministic input, remuxes it with stage journaling
// and prints the lossless summary line.
func runDemo(args []string) error {
	fs := newFlagSet("demo")
	dir := fs.String("dir", "", "work directory")
	tracks := fs.Int("tracks", 3, "number of input tracks")
	samples := fs.Int("samples", 240, "samples per track")
	queue := fs.Int("queue", 8, "bounded sample queue capacity")
	seed := fs.Int64("seed", 0, "fixture seed override")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireDir(fs, *dir); err != nil {
		return err
	}
	if *tracks < 1 || *samples < 1 || *queue < 1 {
		return fmt.Errorf("demo: tracks, samples and queue must all be positive")
	}
	if err := verify.EnsureDir(*dir); err != nil {
		return err
	}
	ctx := context.Background()
	plan := fixture.DefaultPlan(*tracks, *samples)
	if *seed != 0 {
		plan.Seed = *seed
	}
	input := filepath.Join(*dir, "input.mp4")
	if err := os.WriteFile(input, plan.Build(), 0o644); err != nil {
		return fmt.Errorf("demo: write fixture: %w", err)
	}
	r, err := demux.Open(ctx, input)
	if err != nil {
		return err
	}
	defer r.Close()
	mappingPlan, err := mapping.Apply(r.Tracks, mapping.DefaultRules(r.Tracks))
	if err != nil {
		return err
	}
	res, err := txn.Run(ctx, *dir, r, mappingPlan, txn.Options{Queue: *queue, InterruptAfter: -1})
	if err != nil {
		return err
	}
	fmt.Println(res.FormatSummary())
	if res.QueuePeak > *queue+2 {
		return fmt.Errorf("demo: queue peak %d exceeds capacity+2 (%d)", res.QueuePeak, *queue+2)
	}
	return nil
}

// runVerify re-reads a work directory.
func runVerify(args []string) error {
	fs := newFlagSet("verify")
	dir := fs.String("dir", "", "work directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireDir(fs, *dir); err != nil {
		return err
	}
	rep, err := verify.Run(context.Background(), *dir)
	if err != nil {
		return err
	}
	fmt.Println(rep.Summary())
	return nil
}
