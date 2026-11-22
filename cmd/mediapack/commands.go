package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mediapack/internal/checkpoint"
	"mediapack/internal/fixture"
	"mediapack/internal/pipeline"
)

// demoFlags holds parsed `demo` arguments.
type demoFlags struct {
	generate    bool
	out         string
	queueBytes  int64
	watermarkMS int64
	gridMS      int64
	fixtureName string
}

func parseDemo(args []string) (demoFlags, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	var d demoFlags
	fs.BoolVar(&d.generate, "generate", false, "generate self-contained demo material")
	fs.StringVar(&d.out, "out", "", "output working directory (required)")
	fs.Int64Var(&d.queueBytes, "queue-bytes", 1<<20, "bounded queue byte budget")
	fs.Int64Var(&d.watermarkMS, "watermark-ms", 500, "watermark lag in milliseconds")
	fs.Int64Var(&d.gridMS, "grid-ms", 500, "common alignment grid in milliseconds")
	fs.StringVar(&d.fixtureName, "fixture", "valid", "fixture name to generate")
	if err := fs.Parse(args); err != nil {
		return d, err
	}
	if !d.generate {
		return d, fmt.Errorf("demo requires --generate")
	}
	if d.out == "" {
		return d, fmt.Errorf("demo requires --out")
	}
	return d, nil
}

func cmdDemo(args []string) int {
	d, err := parseDemo(args)
	if err != nil {
		return mustOutput("demo arguments", err)
	}
	data, err := loadFixtureData(d.fixtureName)
	if err != nil {
		return mustOutput("demo fixture", err)
	}
	if err := os.MkdirAll(d.out, 0o755); err != nil {
		return mustOutput("demo output", err)
	}
	if _, err := writeSource(d.out, data); err != nil {
		return mustOutput("demo source", err)
	}
	out, err := pipeline.NewOutputs(d.out)
	if err != nil {
		return mustOutput("demo outputs", err)
	}
	store, err := checkpoint.NewStore(filepath.Join(d.out, "state"))
	if err != nil {
		return mustOutput("demo checkpoint", err)
	}
	cfg := pipeline.DefaultConfig()
	cfg.QueueBytes = d.queueBytes
	cfg.WatermarkMS = d.watermarkMS
	cfg.GridMS = d.gridMS

	m, err := pipeline.Run(context.Background(), data, cfg, out, store, nil)
	if err != nil {
		// Even a failed run emits whatever metrics it captured.
		if m != nil {
			printJSON(m)
		}
		fmt.Fprintf(os.Stderr, "demo run failed: %v\n", err)
		return exitFor(err)
	}
	printJSON(m)
	fmt.Printf("\nfixture=%s clusters=%d wrote %s\n",
		d.fixtureName, fixture.Valid().ClusterCount, out.ManifestPath())
	return exitOK
}

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", "", "working directory produced by demo (required)")
	if err := fs.Parse(args); err != nil {
		return mustOutput("verify arguments", err)
	}
	if *dir == "" {
		return mustOutput("verify arguments", fmt.Errorf("verify requires --dir"))
	}
	out, err := pipeline.NewOutputs(*dir)
	if err != nil {
		return mustOutput("verify outputs", err)
	}
	stored, err := out.ReadChecksum()
	if err != nil {
		return mustOutput("verify checksum", err)
	}
	actual, err := out.RecomputeChecksum()
	if err != nil {
		return mustOutput("verify manifest", err)
	}
	result := map[string]any{
		"dir":             *dir,
		"stored_sha256":   stored,
		"computed_sha256": actual,
		"match":           stored == actual,
	}
	printJSON(result)
	if stored != actual {
		fmt.Fprintln(os.Stderr, "checksum mismatch: manifest was modified or is incomplete")
		return exitMismatch
	}
	return exitOK
}

func cmdRecover(args []string) int {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	dir := fs.String("dir", "", "working directory produced by demo (required)")
	interrupt := fs.Int("interrupt-after", 0, "simulate crash after N committed segments")
	if err := fs.Parse(args); err != nil {
		return mustOutput("recover arguments", err)
	}
	if *dir == "" {
		return mustOutput("recover arguments", fmt.Errorf("recover requires --dir"))
	}
	data, err := os.ReadFile(filepath.Join(*dir, "source.webm"))
	if err != nil {
		return mustOutput("recover source", err)
	}
	out, err := pipeline.NewOutputs(*dir)
	if err != nil {
		return mustOutput("recover outputs", err)
	}
	store, err := checkpoint.NewStore(filepath.Join(*dir, "state"))
	if err != nil {
		return mustOutput("recover checkpoint", err)
	}
	cfg := pipeline.DefaultConfig()

	// Optional phase 1: deliberately interrupt to create a checkpoint.
	if *interrupt > 0 {
		cfg1 := cfg
		cfg1.InterruptAfterSegments = *interrupt
		_, ierr := pipeline.Run(context.Background(), data, cfg1, out, store, nil)
		if ierr != nil && !pipeline.IsInterrupted(ierr) {
			return mustOutput("interrupted run", ierr)
		}
	}

	// Phase 2: resume from whatever checkpoint exists.
	if !store.Exists() {
		return mustOutput("recover", fmt.Errorf("no checkpoint found; run with --interrupt-after first"))
	}
	st, err := store.Load()
	if err != nil {
		return mustOutput("recover load", err)
	}
	resume := &pipeline.ResumeState{
		ParseOffset: st.ParseOffset, WatermarkNS: st.WatermarkNS,
		ManifestSHA: st.ManifestSHA, SourceFP: st.SourceFP,
	}
	m, err := pipeline.Run(context.Background(), data, cfg, out, store, resume)
	if err != nil {
		if m != nil {
			printJSON(m)
		}
		return mustOutput("recover run", err)
	}
	printJSON(m)
	return exitOK
}
