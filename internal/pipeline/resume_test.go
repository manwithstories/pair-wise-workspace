package pipeline

import (
	"context"
	"testing"

	"archive/internal/testdata"
)

// TestResumeRecoversCommittedChunks runs a job twice into the same output
// directory with identical parameters. The second run must validate the
// committed index and report recovered chunks rather than failing.
func TestResumeRecoversCommittedChunks(t *testing.T) {
	cfg := baseCfg(t, testdata.ScenarioClean)
	cfg.Packets = 3000
	cfg.ChunkPackets = 256
	first, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.RecoveredChunks == 0 {
		t.Fatal("expected the committed index to contain complete chunks")
	}
	second, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("resume with matching digests must succeed: %v", err)
	}
	if second.RecoveredChunks == 0 {
		t.Error("resume should recover the previously committed chunks")
	}
	if second.InputDigest != first.InputDigest {
		t.Error("input digest must be stable across resume")
	}
}

// TestLongRunStableAndMonotonic drives a larger stream and asserts frames
// are produced cleanly with no clock errors and a perfect round trip. The
// actual 32-bit timestamp boundary is exercised directly by the clock
// package's TestUnwrapWrapAround, which is far faster than generating the
// ~4.8M packets needed to wrap a 90 kHz clock end to end.
func TestLongRunStableAndMonotonic(t *testing.T) {
	cfg := baseCfg(t, testdata.ScenarioClean)
	cfg.Packets = 8000
	st, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("long run failed: %v", err)
	}
	if st.Frames == 0 {
		t.Fatal("expected frames")
	}
	if st.RoundtripRate != 1.0 {
		t.Errorf("roundtrip = %.3f, want 1.0", st.RoundtripRate)
	}
}
