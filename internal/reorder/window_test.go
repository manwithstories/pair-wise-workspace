package reorder

import (
	"testing"

	"mediaroute/internal/adapter"
)

func ev(seq uint32) adapter.Event {
	return adapter.Event{Track: "v", Kind: adapter.KindMedia, Seq: seq}
}

func seqs(evs []adapter.Event) []uint32 {
	out := make([]uint32, len(evs))
	for i, e := range evs {
		out[i] = e.Seq
	}
	return out
}

func TestWindowInOrder(t *testing.T) {
	w := NewWindow(8)
	var got []uint32
	for s := uint32(0); s < 10; s++ {
		got = append(got, seqs(w.Push(ev(s)))...)
	}
	if len(got) != 10 {
		t.Fatalf("got %v", got)
	}
	for i, s := range got {
		if s != uint32(i) {
			t.Fatalf("out of order: %v", got)
		}
	}
}

func TestWindowReorderWithinBounds(t *testing.T) {
	w := NewWindow(8)
	w.Push(ev(0)) // first packet anchors the window, delivered at once
	if out := w.Push(ev(2)); len(out) != 0 {
		t.Fatalf("premature delivery: %v", seqs(out))
	}
	if out := w.Push(ev(3)); len(out) != 0 {
		t.Fatalf("premature delivery: %v", seqs(out))
	}
	out := w.Push(ev(1))
	if got := seqs(out); len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("got %v", got)
	}
}

func TestWindowDuplicateCountedOnce(t *testing.T) {
	w := NewWindow(8)
	w.Push(ev(0))
	w.Push(ev(1))
	w.Push(ev(1)) // duplicate of buffered
	w.Push(ev(0)) // duplicate of delivered
	w.Push(ev(0)) // duplicate again — counted each time it arrives
	if w.Duplicates != 3 {
		t.Fatalf("duplicates = %d, want 3", w.Duplicates)
	}
}

func TestWindowOverflowRecordsGap(t *testing.T) {
	w := NewWindow(4)
	if out := w.Push(ev(10)); len(out) != 1 { // window starts at 10, delivered at once
		t.Fatalf("first push = %v", seqs(out))
	}
	w.Push(ev(12)) // buffered, waiting for 11
	out := w.Push(ev(20))
	if got := seqs(out); len(got) != 2 || got[0] != 12 || got[1] != 20 {
		t.Fatalf("overflow flush = %v", got)
	}
	if len(w.Gaps) != 2 || w.Gaps[0] != (Gap{From: 11, To: 11}) || w.Gaps[1] != (Gap{From: 13, To: 19}) {
		t.Fatalf("gaps = %+v", w.Gaps)
	}
}

func TestWindowFlush(t *testing.T) {
	w := NewWindow(8)
	w.Push(ev(5)) // delivered immediately
	w.Push(ev(7)) // buffered behind missing 6
	out := w.Flush()
	if got := seqs(out); len(got) != 1 || got[0] != 7 {
		t.Fatalf("flush = %v", got)
	}
	if len(w.Gaps) != 1 || w.Gaps[0] != (Gap{From: 6, To: 6}) {
		t.Fatalf("gaps = %+v", w.Gaps)
	}
}

func TestWindowSnapshotRestore(t *testing.T) {
	w := NewWindow(8)
	w.Push(ev(1))
	w.Push(ev(0))
	w.Push(ev(3))
	st := w.Snapshot()
	w2 := NewWindow(8)
	w2.Restore(st)
	a := w.Push(ev(2))
	b := w2.Push(ev(2))
	if len(a) != len(b) || len(a) != 2 || a[1].Seq != 3 || b[1].Seq != 3 {
		t.Fatalf("restore divergence: %v vs %v", seqs(a), seqs(b))
	}
	if w.Duplicates != w2.Duplicates || len(w.Gaps) != len(w2.Gaps) {
		t.Fatal("counters diverged")
	}
}
