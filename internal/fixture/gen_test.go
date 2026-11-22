package fixture

import (
	"testing"

	"mediasync/internal/clock"
)

func testTB() clock.Timebase {
	return clock.Timebase{FPS: clock.Rational{Num: 30000, Den: 1001}}
}

func allFaults() map[string]bool {
	return map[string]bool{FaultDuplicate: true, FaultReorder: true, FaultBurstLoss: true}
}

func TestGenerateDeterministic(t *testing.T) {
	a := Generate(42, testTB(), allFaults())
	b := Generate(42, testTB(), allFaults())
	if len(a.Packets) != len(b.Packets) {
		t.Fatalf("packet counts differ: %d vs %d", len(a.Packets), len(b.Packets))
	}
	for i := range a.Packets {
		if a.Packets[i].SequenceNumber != b.Packets[i].SequenceNumber ||
			a.Packets[i].Timestamp != b.Packets[i].Timestamp {
			t.Fatalf("packet %d differs", i)
		}
	}
	if a.ValidSRT != b.ValidSRT || a.CorruptSRT != b.CorruptSRT {
		t.Fatal("subtitle samples differ")
	}
}

func TestGenerateShape(t *testing.T) {
	s := Generate(42, testTB(), allFaults())
	if s.TotalFrames != 17982 {
		t.Fatalf("total frames = %d, want 17982", s.TotalFrames)
	}
	if len(s.Keyframes) != 300 {
		t.Fatalf("keyframes = %d, want 300", len(s.Keyframes))
	}
	if len(s.LostFrames) != 15 {
		t.Fatalf("lost = %d, want 15 (5 bursts of 3)", len(s.LostFrames))
	}
	// Every lost burst starts on a segment boundary keyframe.
	for f := range s.LostFrames {
		if f%s.SegmentFrames == 0 && f%s.KeyframeInt != 0 {
			t.Fatalf("burst start %d not on keyframe", f)
		}
	}
	// Duplicates inflate the stream beyond unique delivered frames.
	if int64(len(s.Packets)) <= s.TotalFrames-int64(len(s.LostFrames)) {
		t.Fatalf("no duplicates injected: %d packets", len(s.Packets))
	}
}

func TestGenerateNoFaults(t *testing.T) {
	s := Generate(7, testTB(), map[string]bool{})
	if len(s.LostFrames) != 0 || int64(len(s.Packets)) != s.TotalFrames {
		t.Fatalf("faultless sample: packets=%d lost=%d", len(s.Packets), len(s.LostFrames))
	}
}
