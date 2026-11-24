package pipeline

import (
	"errors"
	"os"
	"testing"

	"mediad/internal/model"
)

func TestRefusesToOverwriteUnfinishedOutput(t *testing.T) {
	dir := t.TempDir()
	gen(t, dir, 1, 40, 0, false)
	p := Paths{Dir: dir, Track: 1}
	// Simulate a run that died after publishing an index but before committing
	// any checkpoint: output exists, progress is unprovable.
	if err := os.WriteFile(p.Index(), []byte("half-written index"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run(Options{WorkDir: dir, TrackID: 1, TargetFrames: 40})
	if err == nil {
		t.Fatal("overwrote output with no committed checkpoint")
	}
	if !errors.Is(err, model.ErrUnfinishedOutput) {
		t.Fatalf("err = %v, want ErrUnfinishedOutput", err)
	}
	if model.ExitCode(err) != 2 {
		t.Fatalf("exit = %d, want 2", model.ExitCode(err))
	}
}

func TestAllowsRebuildWhenCheckpointVerifies(t *testing.T) {
	dir := t.TempDir()
	gen(t, dir, 1, 40, 0, false)
	// A first complete run leaves a verified checkpoint.
	if _, err := Run(Options{WorkDir: dir, TrackID: 1, TargetFrames: 40}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// A second run may rebuild the same track because the checkpoint vouches
	// for the existing artifacts.
	if _, err := Run(Options{WorkDir: dir, TrackID: 1, TargetFrames: 40}); err != nil {
		t.Fatalf("rebuild over verified output: %v", err)
	}
}
