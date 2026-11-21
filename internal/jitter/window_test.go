package jitter

import (
	"errors"
	"testing"

	"avcheck/internal/clock"
)

func blk(ssrc uint32, seq uint16, tick int64) *clock.Block {
	return &clock.Block{SSRC: ssrc, Seq: seq, StartTick: tick, Channels: 1,
		Samples: []int32{int32(seq)}}
}

func TestInOrderAndReorderDrain(t *testing.T) {
	w := NewWindow(8)
	for _, s := range []uint16{0, 1, 2} {
		if oc, err := w.Push(blk(1, s, int64(s))); oc != OutcomeAccepted || err != nil {
			t.Fatalf("push %d: oc=%d err=%v", s, oc, err)
		}
	}
	got := w.Drain()
	if len(got) != 3 || got[2].Seq != 2 {
		t.Fatalf("drain = %v", got)
	}

	// Reordered arrival: 5 then 3,4.
	if _, err := w.Push(blk(1, 5, 5)); err != nil {
		t.Fatal(err)
	}
	if len(w.Drain()) != 0 {
		t.Fatal("nothing contiguous should drain while seq 3 is missing")
	}
	w.Push(blk(1, 3, 3))
	w.Push(blk(1, 4, 4))
	got = w.Drain()
	if len(got) != 3 || got[0].Seq != 3 || got[2].Seq != 5 {
		t.Fatalf("reordered drain = %v", got)
	}
}

func TestDuplicateAndWrap(t *testing.T) {
	w := NewWindow(8)
	base := uint16(0xFFFD)
	for i := uint16(0); i < 6; i++ {
		oc, err := w.Push(blk(9, base+i, int64(i)))
		if oc != OutcomeAccepted || err != nil {
			t.Fatalf("wrap push %d: %v", i, err)
		}
	}
	oc, _ := w.Push(blk(9, base+2, 2)) // same seq again after wrap arithmetic
	if oc != OutcomeDuplicate {
		t.Fatalf("expected duplicate, got %d", oc)
	}
	got := w.Drain()
	if len(got) != 6 || got[5].Seq != uint16(0x0002) {
		t.Fatalf("wrap drain len=%d last=%d", len(got), got[len(got)-1].Seq)
	}
	// Re-sending an already drained sequence is late, not duplicate.
	_, err := w.Push(blk(9, base, 0))
	var ce *clock.Error
	if !errors.As(err, &ce) || ce.Kind != clock.KindLate {
		t.Fatalf("err=%v, want late", err)
	}
}

func TestOverflowMarkedLate(t *testing.T) {
	w := NewWindow(4)
	w.Push(blk(1, 0, 0))
	oc, err := w.Push(blk(1, 7, 7)) // distance 7 >= capacity 4
	if oc != OutcomeOverflow || err == nil {
		t.Fatalf("oc=%d err=%v, want overflow + late error", oc, err)
	}
	missing := w.Flush(7)
	if len(missing) != 6 || missing[0] != 1 || missing[5] != 6 {
		t.Fatalf("missing=%v, want seqs 1..6", missing)
	}
	oc, _ = w.Push(blk(1, 7, 7))
	if oc != OutcomeAccepted {
		t.Fatalf("post-flush push oc=%d", oc)
	}
}

func TestStatsAndSnapshot(t *testing.T) {
	w := NewWindow(8)
	w.Push(blk(1, 0, 0))
	w.Push(blk(1, 0, 0)) // duplicate
	w.Push(blk(1, 2, 2))
	if w.Buffered() != 2 {
		t.Fatalf("buffered=%d, want 2", w.Buffered())
	}
	snap := w.Snapshot()
	if snap[0].Seq != 0 || snap[1].Seq != 2 {
		t.Fatalf("snapshot order wrong: %v", snap)
	}
	s := w.Stats()
	if s.Accepted != 2 || s.Duplicates != 1 || s.Late != 0 {
		t.Fatalf("stats=%+v", s)
	}
}
