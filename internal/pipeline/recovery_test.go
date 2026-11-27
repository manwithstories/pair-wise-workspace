package pipeline

import (
	"testing"

	"mediadelivery/internal/fixture"
)

// TestUnrecoverableSlotFlagged ensures a slot missing from both representations
// is marked unrecoverable rather than silently dropped.
func TestUnrecoverableSlotFlagged(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRunner(Config{
		WorkDir: dir, Format: "hls", Cameras: 3, DurationSec: 45,
		Profile: fixture.ProfileBoundary, RunID: "unrec", MaxWorkers: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := r.Run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !m.HasUnrecoverable {
		t.Fatal("boundary profile must contain an unrecoverable slot")
	}
	// Exactly one unrecoverable slot per the deterministic plan.
	if m.RecoveredSlots != 9 || m.LostSlots != 10 {
		t.Fatalf("recovered=%d lost=%d, want 9/10", m.RecoveredSlots, m.LostSlots)
	}
}
