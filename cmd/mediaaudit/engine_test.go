package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediaaudit/internal/fixture"
	"mediaaudit/internal/index"
	"mediaaudit/internal/model"
	"mediaaudit/internal/pipeline"
)

// buildPkg generates a small damaged package for engine-level tests.
func buildPkg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	o := fixture.Options{
		Dir: dir, Seed: 5, Channels: 4, Frames: 96000, SampleRate: 48000,
		TileFrames: 32, DamageFrame: 1024, WrapAt: 4294967000,
		DriftPPM: 180, VFRPattern: []int{2, 3, 1},
	}
	if _, err := fixture.Build(o); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestScanResumeVerifyEndToEnd(t *testing.T) {
	pkg := buildPkg(t)
	idxPath := filepath.Join(t.TempDir(), "i.pfi")
	repPath := filepath.Join(t.TempDir(), "r.json")
	p := pipeline.Params{ChunkFrames: 4096, CheckpointEvery: 8, Workers: 2}

	rep, _, err := runEngine(pkg, idxPath, repPath, p, false)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rep.DamageCount != 1 {
		t.Fatalf("damage=%d want 1", rep.DamageCount)
	}
	if rep.SampleFrames != [2]int64{1024, 1056} {
		t.Fatalf("damage frames=%v want [1024 1056]", rep.SampleFrames)
	}
	if rep.ClockDriftPPM < 150 || rep.ClockDriftPPM > 210 {
		t.Fatalf("drift=%.2f want ~180", rep.ClockDriftPPM)
	}
	// Re-running over a finalized index must not reprocess chunks and must
	// yield the same embedded report checksum.
	rep2, _, err := runEngine(pkg, idxPath, repPath, p, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.ReportSHA256 != rep.ReportSHA256 {
		t.Fatal("report checksum changed on idempotent resume")
	}
}

func TestTornIndexIsRejected(t *testing.T) {
	pkg := buildPkg(t)
	idxPath := filepath.Join(t.TempDir(), "torn.pfi")
	p := pipeline.Params{ChunkFrames: 4096, CheckpointEvery: 4, Workers: 1,
		InterruptAfterCheckpoint: 6}
	_, _, err := runEngine(pkg, idxPath, "", p, false)
	if !pipeline.IsInterrupted(err) {
		t.Fatalf("expected interrupt, got %v", err)
	}
	st, _ := os.Stat(idxPath)
	// Tear away the final third of the file.
	tear := int(st.Size()) * 2 / 3
	if err := index.Tear(idxPath, tear); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Load(idxPath); !errors.Is(err, model.ErrIndexUnrecoverable) {
		t.Fatalf("torn index must be unrecoverable, got %v", err)
	}
}

func TestConfigMismatchBlocksResume(t *testing.T) {
	pkg := buildPkg(t)
	idxPath := filepath.Join(t.TempDir(), "i.pfi")
	p := pipeline.Params{ChunkFrames: 4096, CheckpointEvery: 8, Workers: 1}
	if _, _, err := runEngine(pkg, idxPath, "", p, false); err != nil {
		t.Fatal(err)
	}
	// Re-open and validate against a different chunk size.
	idx, err := index.Load(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	bad := idx.Payload.Config
	bad.ChunkFrames = 2048
	if err := idx.ValidateConfig(bad); !errors.Is(err, model.ErrIndexUnrecoverable) {
		t.Fatalf("config mismatch must block resume, got %v", err)
	}
}

func TestInvalidParamsRejected(t *testing.T) {
	idx, _ := index.Create(filepath.Join(t.TempDir(), "x.pfi"),
		index.Config{ChunkFrames: 10, CheckpointEvery: 1, TileFrames: 32, InputFingerprint: "f"}, 100)
	if _, err := pipeline.NewRunner(idx, pipeline.Params{ChunkFrames: 0, CheckpointEvery: 1}); err == nil {
		t.Fatal("zero chunk frames must be rejected")
	}
	if _, err := pipeline.NewRunner(idx, pipeline.Params{ChunkFrames: 10, CheckpointEvery: 0}); err == nil {
		t.Fatal("zero checkpoint interval must be rejected")
	}
}
