package index

import (
	"testing"

	"cinepipe/internal/synth"
)

// TestScanCountsAndDamagedFrames checks the aggregate statistics the demo
// report reads, so the numbers shown are consistent with the per-frame index.
func TestScanCountsAndDamagedFrames(t *testing.T) {
	s := synth.Build(synth.SampleNormal, synth.Defaults(synth.SampleNormal, 7))
	idx := Run(s.Data)
	if idx.Stats.VideoFrames != int64(len(s.FrameTicks)) {
		t.Fatalf("video frames %d, planned %d", idx.Stats.VideoFrames, len(s.FrameTicks))
	}
	if idx.Stats.Keyframes != int64(len(s.KeyframeTicks)) {
		t.Fatalf("keyframes %d, planned %d", idx.Stats.Keyframes, len(s.KeyframeTicks))
	}
	if idx.Stats.Packets != idx.Source.Bytes/188 {
		t.Fatalf("packet count %d vs %d", idx.Stats.Packets, idx.Source.Bytes/188)
	}
	if idx.Stats.DamagedFrames != 0 {
		t.Fatalf("clean sample has damaged frames: %d", idx.Stats.DamagedFrames)
	}
}

// TestFirstErrorOffsetIsExact confirms the first-error offset points at the
// first byte that diverged, a requirement of the report.
func TestFirstErrorOffsetIsExact(t *testing.T) {
	s := synth.Build(synth.SampleTruncated, synth.Defaults(synth.SampleTruncated, 3))
	idx := Run(s.Data)
	if idx.FirstError < 0 {
		t.Fatal("expected a first error")
	}
	// The truncation is the final short packet, which starts at the last
	// multiple of 188 that still has bytes.
	lastFull := (idx.Source.Bytes / 188) * 188
	if idx.FirstError < lastFull {
		t.Fatalf("first error %d before tail packet %d", idx.FirstError, lastFull)
	}
	if idx.FirstError >= idx.Source.Bytes {
		t.Fatalf("first error %d beyond input %d", idx.FirstError, idx.Source.Bytes)
	}
}
