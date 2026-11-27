package pipeline

import (
	"testing"

	"mediadelivery/internal/fixture"
)

// TestRecoveryMetricsFromDurableEntries guards against the regression where a
// resumed run under-counts recovery because the in-memory fault recorder lost
// markers for blocks skipped via checkpoint.
func TestRecoveryMetricsFromDurableEntries(t *testing.T) {
	// Full run.
	fullDir := t.TempDir()
	full, err := NewRunner(Config{WorkDir: fullDir, Format: "both", Cameras: 3,
		DurationSec: 45, Profile: fixture.ProfileBoundary, RunID: "m", MaxWorkers: 3})
	if err != nil {
		t.Fatal(err)
	}
	fm, err := full.Run()
	if err != nil {
		t.Fatal(err)
	}

	// Interrupted then resumed run.
	intDir := t.TempDir()
	cfg := Config{WorkDir: intDir, Format: "both", Cameras: 3, DurationSec: 45,
		Profile: fixture.ProfileBoundary, RunID: "m", MaxWorkers: 3, InterruptAfterBlock: 4}
	r1, _ := NewRunner(cfg)
	if _, err := r1.Run(); err != ErrInterrupted {
		t.Fatalf("want interruption, got %v", err)
	}
	cfg.InterruptAfterBlock = 0
	r2, _ := NewRunner(cfg)
	rm, err := r2.Run()
	if err != nil {
		t.Fatal(err)
	}

	if rm.LostSlots != fm.LostSlots || rm.LostSlots != 10 {
		t.Fatalf("lost slots full=%d resume=%d", fm.LostSlots, rm.LostSlots)
	}
	if rm.RecoveredSlots != fm.RecoveredSlots || rm.RecoveredSlots != 9 {
		t.Fatalf("recovered full=%d resume=%d", fm.RecoveredSlots, rm.RecoveredSlots)
	}
	if rm.LossRecoveryRate != 0.9 || fm.LossRecoveryRate != 0.9 {
		t.Fatalf("recovery rate full=%v resume=%v", fm.LossRecoveryRate, rm.LossRecoveryRate)
	}
}
