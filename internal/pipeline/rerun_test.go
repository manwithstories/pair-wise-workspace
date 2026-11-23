package pipeline

import "testing"

// TestRerunInPlaceIsIdentical runs the same configuration twice in one work
// directory without resuming. The second run must reproduce the exact same
// final manifest, proving the WAL and checkpoints do not perturb output.
func TestRerunInPlaceIsIdentical(t *testing.T) {
	cfg := Config{
		Seed: 42, Segments: 3, SegmentDurNS: 2_000_000_000, PacketRate: 50,
		WorkDir: t.TempDir(), Faults: Faults{Drop: true},
	}
	first, err := Run(cfg)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := Run(cfg)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if first.NormalHash != second.NormalHash {
		t.Fatalf("rerun differs: %s vs %s", first.NormalHash, second.NormalHash)
	}
}
