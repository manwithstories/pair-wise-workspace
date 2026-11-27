package pipeline

import (
	"testing"

	"mediadelivery/internal/fixture"
)

func TestRunCleanProfile(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRunner(Config{
		WorkDir: dir, Format: "hls", Cameras: 2,
		DurationSec: 20, Profile: fixture.ProfileClean, RunID: "clean",
		MaxWorkers: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := r.Run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if m.LostSlots != 0 {
		t.Fatalf("clean run lost slots = %d", m.LostSlots)
	}
	if m.ProcessedBlocks == 0 {
		t.Fatal("no blocks processed")
	}
}

func TestRunBoundaryRecovery(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRunner(Config{
		WorkDir: dir, Format: "hls", Cameras: 3,
		DurationSec: 45, Profile: fixture.ProfileBoundary, RunID: "boundary",
		MaxWorkers: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := r.Run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if m.LostSlots != 10 {
		t.Fatalf("lost = %d, want 10", m.LostSlots)
	}
	if m.RecoveredSlots != 9 {
		t.Fatalf("recovered = %d, want 9", m.RecoveredSlots)
	}
	if m.LossRecoveryRate != 0.9 {
		t.Fatalf("recovery rate = %v, want 0.9", m.LossRecoveryRate)
	}
	if !m.HasUnrecoverable {
		t.Fatal("expected an unrecoverable slot")
	}
}

func TestRunResumeHashStable(t *testing.T) {
	base := func(dir string, interrupt int) Config {
		return Config{
			WorkDir: dir, Format: "hls", Cameras: 2,
			DurationSec: 20, Profile: fixture.ProfileClean, RunID: "resume",
			MaxWorkers: 2, InterruptAfterBlock: interrupt,
		}
	}

	// Reference: a single uninterrupted run.
	fullDir := t.TempDir()
	rf, err := NewRunner(base(fullDir, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rf.Run(); err != nil {
		t.Fatalf("full run: %v", err)
	}
	fullHash := rf.IndexHash()

	// Interrupted run followed by a resume into the same working directory.
	intDir := t.TempDir()
	r1, err := NewRunner(base(intDir, 4))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r1.Run(); err != ErrInterrupted {
		t.Fatalf("expected interruption, got %v", err)
	}

	r2, err := NewRunner(base(intDir, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Run(); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if r2.IndexHash() == "" || r2.IndexHash() != fullHash {
		t.Fatalf("resume hash %q != full hash %q", r2.IndexHash(), fullHash)
	}
}
