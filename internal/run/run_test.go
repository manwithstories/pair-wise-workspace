package run

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"review/internal/clock"
	"review/internal/fixture"
)

func setup(t *testing.T, cams int) (string, Config) {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{Cams: cams, FPS: "30000/1001", BlockFrames: 30,
		FramesPerCam: 120, DayWrap: true,
		Inject: []string{"truncated-srt", "damaged-mp4", "incomplete-index"}}
	rate, _ := clock.ParseRate(cfg.FPS)
	if err := fixture.Generate(dir, cfg.Cams, rate, cfg.FramesPerCam, cfg.Inject); err != nil {
		t.Fatal(err)
	}
	return dir, cfg
}

func TestInterruptResumeConsistency(t *testing.T) {
	dir, cfg := setup(t, 4)
	var sig atomic.Bool

	// Interrupted run: stops after 2 new blocks, stays resumable.
	s1, err := Run(dir, cfg, 2, &sig)
	if err != nil {
		t.Fatal(err)
	}
	if s1.Status != "interrupted" || s1.BlocksDone == 0 || s1.BlocksDone >= s1.BlocksTotal {
		t.Fatalf("bad interrupted state: %+v", s1)
	}
	// Resume to completion.
	s2, err := Run(dir, cfg, 0, &sig)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Status != "complete" || s2.RecoveredBlocks != s1.BlocksDone {
		t.Fatalf("bad resume: %+v", s2)
	}
	// A second resume is a pure verification pass with the same hash.
	s3, err := Run(dir, cfg, 0, &sig)
	if err != nil {
		t.Fatal(err)
	}
	if s3.SHA256 != s2.SHA256 || len(s3.SHA256) != 64 {
		t.Fatalf("hash changed across resumes: %s vs %s", s2.SHA256, s3.SHA256)
	}
	if s2.FramesExported != 4*120 || s3.FramesExported != s2.FramesExported {
		t.Fatalf("frames: %d %d", s2.FramesExported, s3.FramesExported)
	}
	if s2.AlignmentErrorFrames < 0 || s2.AlignmentErrorFrames > 2 {
		t.Fatalf("alignment errors: %d", s2.AlignmentErrorFrames)
	}
	if s2.DamageOffsetBytes < 0 {
		t.Fatal("damage offset not reported")
	}
	if s2.ExitCode() != 2 {
		t.Fatalf("exit code %d, want 2 (located errors)", s2.ExitCode())
	}
	// Uninterrupted reference run on identical inputs must match.
	dir2, cfg2 := setup(t, 4)
	s4, err := Run(dir2, cfg2, 0, &sig)
	if err != nil {
		t.Fatal(err)
	}
	if s4.SHA256 != s2.SHA256 {
		t.Fatalf("resumed output differs from first run: %s vs %s", s2.SHA256, s4.SHA256)
	}
}

func TestCorruptCheckpointFailsVerification(t *testing.T) {
	dir, cfg := setup(t, 1)
	var sig atomic.Bool
	if _, err := Run(dir, cfg, 0, &sig); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.json"), []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Run(dir, cfg, 0, &sig)
	if err != nil {
		t.Fatal(err)
	}
	if !s.VerificationFailed || s.ExitCode() != 3 {
		t.Fatalf("corrupt checkpoint must fail verification: %+v", s)
	}
}

func TestInputChangeRecomputes(t *testing.T) {
	dir, cfg := setup(t, 1)
	var sig atomic.Bool
	s1, err := Run(dir, cfg, 0, &sig)
	if err != nil {
		t.Fatal(err)
	}
	// Add a cue to an input: fingerprint mismatch -> full recompute, and
	// the new cue shifts the source mapping, so the hash must change.
	f := filepath.Join(dir, "inputs", "cam00.vtt")
	data, _ := os.ReadFile(f)
	os.WriteFile(f, append(data, []byte("extra\n00:00:00.200 --> 00:00:00.900\nnew cue\n\n")...), 0o644)
	s2, err := Run(dir, cfg, 0, &sig)
	if err != nil {
		t.Fatal(err)
	}
	if s2.RecoveredBlocks != 0 {
		t.Fatalf("recovered %d blocks after input change", s2.RecoveredBlocks)
	}
	if s2.SHA256 == s1.SHA256 {
		t.Fatal("hash must change when inputs change")
	}
}
