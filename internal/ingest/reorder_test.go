package ingest

import "testing"

func pushAll(r *Reorderer, items ...Item) []Item {
	var out []Item
	for _, it := range items {
		out = append(out, r.Push(it)...)
	}
	return out
}

func seqs(items []Item) []int64 {
	var out []int64
	for _, it := range items {
		out = append(out, it.Seq)
	}
	return out
}

func TestReorderWithinWindow(t *testing.T) {
	r := NewReorderer(3)
	out := pushAll(r, Item{Seq: 0, CRC: 1}, Item{Seq: 2, CRC: 3}, Item{Seq: 1, CRC: 2})
	out = append(out, r.Flush()...)
	got := seqs(out)
	want := []int64{0, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if len(r.Gaps) != 0 || r.Duplicates != 0 || len(r.Conflicts) != 0 {
		t.Fatalf("unexpected stats: %+v", r)
	}
}

func TestDuplicateAndConflict(t *testing.T) {
	r := NewReorderer(3)
	pushAll(r, Item{Seq: 0, CRC: 10})
	pushAll(r, Item{Seq: 0, CRC: 10}) // identical retransmission
	pushAll(r, Item{Seq: 0, CRC: 99}) // conflicting retransmission
	if r.Duplicates != 1 {
		t.Fatalf("duplicates=%d want 1", r.Duplicates)
	}
	if len(r.Conflicts) != 1 || r.Conflicts[0] != 0 {
		t.Fatalf("conflicts=%v want [0]", r.Conflicts)
	}
}

func TestGapTimeoutAndExpired(t *testing.T) {
	r := NewReorderer(3)
	var out []Item
	out = append(out, pushAll(r, Item{Seq: 0, CRC: 1})...)
	out = append(out, pushAll(r, Item{Seq: 1, CRC: 2})...)
	out = append(out, pushAll(r, Item{Seq: 5, CRC: 6})...) // 2 missing; window hit at 5
	out = append(out, pushAll(r, Item{Seq: 2, CRC: 3})...) // too late for seq 2
	out = append(out, r.Flush()...)
	got := seqs(out)
	if len(got) != 3 || got[0] != 0 || got[1] != 1 || got[2] != 5 {
		t.Fatalf("got %v want [0 1 5]", got)
	}
	if len(r.Gaps) == 0 || r.Gaps[0] != 2 {
		t.Fatalf("gaps=%v want first gap at 2", r.Gaps)
	}
	if r.Expired != 1 {
		t.Fatalf("expired=%d want 1", r.Expired)
	}
}

func TestSessionGapBefore(t *testing.T) {
	s := NewSession("480p", 3)
	s.Push(Item{Seq: 0, CRC: 1})
	s.Push(Item{Seq: 3, CRC: 4})
	s.Push(Item{Seq: 4, CRC: 5})
	s.Close()
	gb := s.GapBefore()
	if len(gb) != 3 || gb[0] != 0 || gb[1] != 2 || gb[2] != 0 {
		t.Fatalf("gapBefore=%v want [0 2 0]", gb)
	}
}
