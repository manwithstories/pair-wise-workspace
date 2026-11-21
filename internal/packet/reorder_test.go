package packet

import (
	"testing"

	"github.com/pion/rtp"

	"mediaroute/internal/model"
)

func mk(seq uint16, ts uint32, payload ...byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts},
		Payload: payload}
}

func run(t *testing.T, r *Reorderer, pkts ...*rtp.Packet) ([]*rtp.Packet, Stats) {
	t.Helper()
	var out []*rtp.Packet
	for _, p := range pkts {
		em, err := r.Push(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, em...)
	}
	rest, st := r.Flush()
	return append(out, rest...), st
}

func seqs(out []*rtp.Packet) []uint16 {
	var s []uint16
	for _, p := range out {
		s = append(s, p.SequenceNumber)
	}
	return s
}

func TestDedupAndReorder(t *testing.T) {
	r := NewReorderer("v1", 4)
	out, st := run(t, r,
		mk(0, 0, 'a'), mk(2, 20, 'c'), mk(1, 10, 'b'),
		mk(3, 30, 'd'), mk(3, 30, 'd'), mk(4, 40, 'e'))
	if st.Duplicates != 1 {
		t.Fatalf("duplicates=%d want 1", st.Duplicates)
	}
	if st.Reordered != 1 {
		t.Fatalf("reordered=%d want 1", st.Reordered)
	}
	got := seqs(out)
	if len(got) != 5 {
		t.Fatalf("duplicate written to timeline: %v", got)
	}
	for i, want := range []uint16{0, 1, 2, 3, 4} {
		if got[i] != want {
			t.Fatalf("order %v", got)
		}
	}
}

func TestDuplicateConflict(t *testing.T) {
	r := NewReorderer("v1", 4)
	if _, err := r.Push(mk(0, 0, 'a')); err != nil {
		t.Fatal(err)
	}
	_, err := r.Push(mk(0, 0, 'X'))
	me, ok := err.(*model.MediaError)
	if !ok || me.Code != model.ErrDuplicate || me.Track != "v1" || me.Seq != 0 {
		t.Fatalf("want duplicate conflict, got %v", err)
	}
}

func TestSequenceWrap(t *testing.T) {
	r := NewReorderer("v1", 4)
	out, st := run(t, r,
		mk(65534, 0, 'a'), mk(65535, 10, 'b'), mk(0, 20, 'c'), mk(1, 30, 'd'))
	if len(st.Gaps) != 0 {
		t.Fatalf("wrap misread as gap: %+v", st.Gaps)
	}
	got := seqs(out)
	for i, want := range []uint16{65534, 65535, 0, 1} {
		if got[i] != want {
			t.Fatalf("wrap order %v", got)
		}
	}
}

func TestGapRecorded(t *testing.T) {
	r := NewReorderer("v1", 2)
	_, st := run(t, r,
		mk(0, 0, 'a'), mk(1, 10, 'b'), mk(8, 80, 'i'), mk(9, 90, 'j'))
	if len(st.Gaps) != 1 || st.Gaps[0].From != 2 || st.Gaps[0].To != 7 {
		t.Fatalf("gaps=%+v want [2,7]", st.Gaps)
	}
	if st.Gaps[0].Track != "v1" {
		t.Fatalf("gap missing track context")
	}
}

func TestLateArrivalFillsGap(t *testing.T) {
	r := NewReorderer("v1", 1)
	var out []*rtp.Packet
	for _, p := range []*rtp.Packet{mk(0, 0, 'a'), mk(2, 20, 'c'), mk(3, 30, 'd')} {
		em, err := r.Push(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, em...)
	}
	em, err := r.Push(mk(1, 10, 'b')) // late, after gap was skipped
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, em...)
	_, st := r.Flush()
	if len(st.Gaps) != 0 {
		t.Fatalf("late arrival should clear gap: %+v", st.Gaps)
	}
	if st.Reordered != 1 {
		t.Fatalf("reordered=%d want 1", st.Reordered)
	}
}
