package ingest

import (
	"testing"

	"mediacheck/internal/model"
)

func mkPacket(seq uint32, start, end int64) model.Packet {
	return model.Packet{
		Track:    model.TrackAudio,
		Sequence: seq,
		PTS:      int64(seq),
		Duration: 1,
		Slot:     model.Slot{Start: start, End: end},
		Data:     []byte{1},
	}
}

func TestSequenceTrackerSteady(t *testing.T) {
	tr := NewSequenceTracker(model.TrackAudio)
	for i := uint32(0); i < 100; i++ {
		res := tr.Observe(i)
		if res.Duplicate || res.Lost != 0 || res.Offset != 0 || res.Wrapped {
			t.Fatalf("seq %d unexpectedly flagged: %+v", i, res)
		}
	}
	if tr.Accepted != 100 || tr.Lost != 0 || tr.Duplicates != 0 {
		t.Fatalf("tracker counters = %+v", tr)
	}
}

func TestSequenceTrackerDuplicateDroppedOncePerRepeat(t *testing.T) {
	tr := NewSequenceTracker(model.TrackAudio)
	var dups int
	for _, seq := range []uint32{0, 1, 2, 2, 3, 3, 3, 4} {
		if tr.Observe(seq).Duplicate {
			dups++
		}
	}
	// Sequence 2 repeats once, sequence 3 repeats twice.
	if dups != 3 {
		t.Fatalf("duplicates = %d, want 3", dups)
	}
	if tr.Duplicates != 3 {
		t.Fatalf("tracker duplicates = %d, want 3", tr.Duplicates)
	}
	if tr.Lost != 0 {
		t.Fatalf("lost = %d, want 0", tr.Lost)
	}
}

func TestSequenceTrackerDetectsLoss(t *testing.T) {
	tr := NewSequenceTracker(model.TrackAudio)
	for _, seq := range []uint32{0, 1, 2, 6} {
		res := tr.Observe(seq)
		if seq == 6 && res.Lost != 3 {
			t.Fatalf("lost at seq 6 = %d, want 3", res.Lost)
		}
	}
	if tr.Lost != 3 {
		t.Fatalf("total lost = %d, want 3", tr.Lost)
	}
}

func TestSequenceTrackerWrapAt65536(t *testing.T) {
	tr := NewSequenceTracker(model.TrackAudio)
	for i := uint32(65530); i <= 65535; i++ {
		tr.Observe(i)
	}
	res := tr.Observe(0)
	if res.Duplicate {
		t.Fatal("wrapped sequence treated as duplicate")
	}
	if res.Lost != 0 {
		t.Fatalf("lost over wrap = %d, want 0", res.Lost)
	}
	if tr.WrapCount != 1 {
		t.Fatalf("wrap count = %d, want 1", tr.WrapCount)
	}
	if tr.Base() != 65536 {
		t.Fatalf("base after wrap = %d, want 65536", tr.Base())
	}
	res = tr.Observe(1)
	if res.Lost != 0 || res.Duplicate {
		t.Fatalf("post-wrap seq 1 flagged: %+v", res)
	}
}

func TestSequenceTrackerWrapThenLoss(t *testing.T) {
	tr := NewSequenceTracker(model.TrackAudio)
	for i := uint32(65533); i <= 65535; i++ {
		tr.Observe(i)
	}
	res := tr.Observe(3) // 0,1,2 lost
	if res.Lost != 3 {
		t.Fatalf("lost after wrap = %d, want 3", res.Lost)
	}
	if tr.WrapCount != 1 {
		t.Fatalf("wrap count = %d", tr.WrapCount)
	}
}

func TestSequenceTrackerRecentRepeatIsDuplicate(t *testing.T) {
	tr := NewSequenceTracker(model.TrackAudio)
	for i := uint32(0); i < 10; i++ {
		tr.Observe(i)
	}
	// Sequence 4 is still inside the dedup window, so it is a retransmit.
	if res := tr.Observe(4); !res.Duplicate {
		t.Fatalf("recent repeat not treated as duplicate: %+v", res)
	}
	if tr.Regressions != 0 {
		t.Fatalf("regressions = %d, want 0", tr.Regressions)
	}
	// Scanning continues: the next forward sequence is accepted normally.
	if res := tr.Observe(11); res.Lost != 1 {
		t.Fatalf("after repeat lost = %d, want 1", res.Lost)
	}
}

func TestSequenceTrackerFarBackwardJumpIsRegression(t *testing.T) {
	tr := NewSequenceTracker(model.TrackAudio)
	for i := uint32(0); i < 1000; i++ {
		tr.Observe(i)
	}
	// A sequence far below the high-water mark that is no longer remembered
	// is a regression, not a retransmit.
	res := tr.Observe(100)
	if res.Duplicate {
		t.Fatal("stale sequence treated as duplicate")
	}
	if res.Offset >= 0 {
		t.Fatalf("regression offset = %d, want negative", res.Offset)
	}
	if tr.Regressions != 1 {
		t.Fatalf("regressions = %d, want 1", tr.Regressions)
	}
}

func TestExpectedSlotsFillsGap(t *testing.T) {
	prev := mkPacket(10, 0, 20)
	next := mkPacket(14, 80, 100)
	slots := ExpectedSlots(prev, next, 3)
	if len(slots) != 3 {
		t.Fatalf("slots = %d, want 3", len(slots))
	}
	if slots[0].Start != 20 || slots[len(slots)-1].End != 80 {
		t.Fatalf("gap slots do not span the hole: %+v", slots)
	}
	for i := 1; i < len(slots); i++ {
		if slots[i].Start != slots[i-1].End {
			t.Fatalf("slot %d not contiguous: %+v", i, slots)
		}
	}
}

func TestExpectedSlotsFallsBackToPreviousDuration(t *testing.T) {
	prev := mkPacket(1, 0, 20)
	next := mkPacket(3, 20, 40) // no visible gap between slots
	slots := ExpectedSlots(prev, next, 1)
	if len(slots) != 1 || slots[0] != (model.Slot{Start: 20, End: 40}) {
		t.Fatalf("fallback slots = %+v", slots)
	}
}
