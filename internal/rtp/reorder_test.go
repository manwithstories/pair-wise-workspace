package rtp

import (
	"testing"

	"archivearchivist/internal/model"
)

func pkts(seqs ...uint16) []Packet {
	out := make([]Packet, len(seqs))
	for i, s := range seqs {
		out[i] = Packet{Seq: s}
	}
	return out
}

func TestReorderInOrder(t *testing.T) {
	rep := Reorder(pkts(1, 2, 3, 4), Options{})
	if rep.Accepted != 4 || rep.Missing != 0 || rep.OutOfOrder != 0 || rep.Duplicates != 0 {
		t.Fatalf("unexpected report: %+v", rep)
	}
	if rep.RecoveryRate().Cmp(model.NewRat(1, 1)) != 0 {
		t.Fatalf("rate = %s", rep.RecoveryRate())
	}
}

func TestReorderOutOfOrderWithinWindow(t *testing.T) {
	rep := Reorder(pkts(1, 3, 2, 4, 6, 5), Options{Window: 8})
	if rep.OutOfOrder != 2 {
		t.Fatalf("out of order = %d, want 2", rep.OutOfOrder)
	}
	if rep.Missing != 0 {
		t.Fatalf("missing = %d, want 0", rep.Missing)
	}
	for i, p := range rep.Stream {
		if int(p.Seq) != i+1 {
			t.Fatalf("stream not sorted at %d: %+v", i, rep.Stream)
		}
	}
}

func TestReorderDedup(t *testing.T) {
	rep := Reorder(pkts(1, 2, 2, 3, 3, 3), Options{})
	if rep.Duplicates != 3 {
		t.Fatalf("duplicates = %d, want 3", rep.Duplicates)
	}
	if rep.Accepted != 3 {
		t.Fatalf("accepted = %d, want 3", rep.Accepted)
	}
}

func TestLateOutsideWindowIsUnrecoverable(t *testing.T) {
	// Sequence 7 arrives after the head advanced to 10, more than the window
	// (2) behind it; it is dropped as unrecoverable.
	rep := Reorder(pkts(1, 2, 3, 4, 5, 10, 7), Options{Window: 2})
	if rep.Accepted != 6 {
		t.Fatalf("accepted = %d, want 6", rep.Accepted)
	}
	if rep.Late != 1 {
		t.Fatalf("late = %d, want 1", rep.Late)
	}
	if rep.Missing != 4 { // 6,7,8,9
		t.Fatalf("missing = %d, want 4 (%v)", rep.Missing, rep.MissingSeqs)
	}
}

func TestLossCountsGap(t *testing.T) {
	rep := Reorder(pkts(1, 2, 5, 6), Options{})
	if rep.Missing != 2 {
		t.Fatalf("missing = %d, want 2", rep.Missing)
	}
	if rep.MissingSeqs[0] != 3 || rep.MissingSeqs[1] != 4 {
		t.Fatalf("missing seqs = %v", rep.MissingSeqs)
	}
	// 4 accepted of 6 expected.
	if got, want := rep.RecoveryRate().String(), "2/3"; got != want {
		t.Fatalf("rate = %s, want %s", got, want)
	}
}

func TestSequenceWraparound(t *testing.T) {
	rep := Reorder(pkts(65534, 65535, 0, 1, 2), Options{})
	if rep.Missing != 0 {
		t.Fatalf("missing = %d (%v)", rep.Missing, rep.MissingSeqs)
	}
	if rep.Accepted != 5 {
		t.Fatalf("accepted = %d", rep.Accepted)
	}
	if rep.LastSeq != 2 {
		t.Fatalf("last = %d, want 2", rep.LastSeq)
	}
}

func TestSequenceWraparoundLate(t *testing.T) {
	// 65535 is late once the head has advanced past 2 by more than the window.
	rep := Reorder(pkts(65533, 65534, 0, 1, 2, 3, 65535), Options{Window: 2})
	if rep.Accepted != 6 {
		t.Fatalf("accepted = %d, want 6", rep.Accepted)
	}
	if rep.Missing != 1 {
		t.Fatalf("missing = %d (%v)", rep.Missing, rep.MissingSeqs)
	}
}

func TestEmptyCapture(t *testing.T) {
	rep := Reorder(nil, Options{})
	if rep.Accepted != 0 || rep.Missing != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if !rep.Faults.Has("packet_loss") {
		t.Fatalf("expected packet_loss fault")
	}
}

// TestLongSpanLossIsCounted guards against 16-bit arithmetic wrapping the
// observed span: a real hole 40000 packets in must still be reported.
func TestLongSpanLossIsCounted(t *testing.T) {
	const n = 50000
	pkts := make([]Packet, 0, n)
	for i := 0; i < n; i++ {
		if i == 20000 {
			continue // the hole
		}
		pkts = append(pkts, Packet{Seq: uint16(1000 + i)})
	}
	first := Reorder(pkts, Options{Window: 64})
	if first.Missing != 1 {
		t.Fatalf("missing = %d (%v), want 1", first.Missing, first.MissingSeqs)
	}
	if first.Accepted != n-1 {
		t.Fatalf("accepted = %d, want %d", first.Accepted, n-1)
	}
	// Determinism: the same input must give the same report every time.
	for i := 0; i < 5; i++ {
		again := Reorder(pkts, Options{Window: 64})
		if again.Missing != first.Missing || again.LastSeq != first.LastSeq || again.Accepted != first.Accepted {
			t.Fatalf("run %d diverged: %+v vs first missing=%d last=%d", i, again, first.Missing, first.LastSeq)
		}
	}
}

// TestLateRetransmitIsNotDoubleCounted checks that a late packet arriving
// twice counts as one duplicate, not two late losses.
func TestLateRetransmitIsNotDoubleCounted(t *testing.T) {
	rep := Reorder(pkts(1, 2, 3, 4, 5, 10, 7, 7), Options{Window: 2})
	if rep.Late != 1 {
		t.Fatalf("late = %d, want 1", rep.Late)
	}
	if rep.Duplicates != 1 {
		t.Fatalf("duplicates = %d, want 1", rep.Duplicates)
	}
}
