package rtprepair

import (
	"testing"

	"github.com/pion/rtp"
)

func pkt(seq uint16) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq}}
}

// feed pushes packets and collects all items including the final flush.
func feed(w *Window, seqs ...uint16) []Item {
	var out []Item
	for _, s := range seqs {
		out = append(out, w.Push(pkt(s))...)
	}
	return append(out, w.Flush()...)
}

func deliveredSeqs(items []Item) []uint16 {
	var got []uint16
	for _, it := range items {
		if it.Packet != nil {
			got = append(got, it.Packet.SequenceNumber)
		}
	}
	return got
}

func missingSeqs(items []Item) []uint16 {
	var got []uint16
	for _, it := range items {
		got = append(got, it.Missing...)
	}
	return got
}

func TestInOrder(t *testing.T) {
	w := New(8)
	items := feed(w, 1, 2, 3, 4)
	got := deliveredSeqs(items)
	if len(got) != 4 || got[0] != 1 || got[3] != 4 {
		t.Fatalf("delivered %v", got)
	}
	if w.Stats.Lost != 0 || w.Stats.Duplicates != 0 {
		t.Fatalf("stats %+v", w.Stats)
	}
}

func TestDuplicatesDropped(t *testing.T) {
	w := New(8)
	items := feed(w, 1, 2, 2, 3, 3, 3)
	if got := deliveredSeqs(items); len(got) != 3 {
		t.Fatalf("delivered %v", got)
	}
	if w.Stats.Duplicates != 3 {
		t.Fatalf("duplicates = %d, want 3", w.Stats.Duplicates)
	}
}

func TestReorderWithinWindow(t *testing.T) {
	w := New(8)
	items := feed(w, 1, 3, 2, 5, 4, 6)
	got := deliveredSeqs(items)
	want := []uint16{1, 2, 3, 4, 5, 6}
	if len(got) != len(want) {
		t.Fatalf("delivered %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delivered %v, want %v", got, want)
		}
	}
	if w.Stats.Reordered == 0 {
		t.Fatal("reordered packets not counted")
	}
}

func TestBurstLossMarkedNotFabricated(t *testing.T) {
	w := New(4)
	// 10,11,12 lost in a burst; stream resumes at 13.
	items := feed(w, 8, 9, 13, 14, 15, 16)
	got := deliveredSeqs(items)
	for _, s := range got {
		if s >= 10 && s <= 12 {
			t.Fatalf("fabricated lost packet %d", s)
		}
	}
	miss := missingSeqs(items)
	if len(miss) != 3 || miss[0] != 10 || miss[2] != 12 {
		t.Fatalf("missing = %v, want [10 11 12]", miss)
	}
	if w.Stats.Lost != 3 {
		t.Fatalf("lost = %d, want 3", w.Stats.Lost)
	}
}

func TestStatsAccounting(t *testing.T) {
	w := New(4)
	feed(w, 1, 2, 2, 4, 3, 7, 8) // 5,6 lost
	s := w.Stats
	if s.Received != 7 || s.Delivered != 6 || s.Lost != 2 || s.Duplicates != 1 {
		t.Fatalf("stats %+v", s)
	}
}
