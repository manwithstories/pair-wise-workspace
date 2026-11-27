package pipeline

import (
	"errors"
	"testing"

	"mediadelivery/internal/fixture"
)

// TestBlockMemoryCapRejectsOversize ensures a block exceeding the per-block
// memory ceiling is rejected rather than loaded.
func TestBlockMemoryCapRejectsOversize(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRunner(Config{
		WorkDir: dir, Format: "hls", Cameras: 1, DurationSec: 4,
		Profile: fixture.ProfileClean, RunID: "cap", MaxWorkers: 1,
		MaxBlockBytes: 1, // unrealistically tight: every segment exceeds it
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Run()
	var oversize *OversizeError
	if !errors.As(err, &oversize) {
		t.Fatalf("expected OversizeError, got %v", err)
	}
}
