package ingest

import (
	"fmt"
	"testing"

	"mediasync/internal/defect"
)

func encode(t *testing.T, seq uint16, tag string) []byte {
	t.Helper()
	b, err := Encode(seq, uint32(seq)*160, 0x15539, []byte(tag))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func collect(t *testing.T, r *Reassembler, seqs ...uint16) []string {
	t.Helper()
	var got []string
	for _, s := range seqs {
		outs, err := r.Push(encode(t, s, fmt.Sprintf("p%d", s)))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range outs {
			got = append(got, string(p))
		}
	}
	return got
}

func TestInOrder(t *testing.T) {
	r := NewReassembler(&LossTracker{})
	got := collect(t, r, 0, 1, 2)
	if fmt.Sprint(got) != "[p0 p1 p2]" {
		t.Fatalf("got %v", got)
	}
}

func TestDuplicateNotDoubleCounted(t *testing.T) {
	loss := &LossTracker{}
	r := NewReassembler(loss)
	got := collect(t, r, 0, 1, 1, 2)
	if fmt.Sprint(got) != "[p0 p1 p2]" {
		t.Fatalf("got %v", got)
	}
	if loss.Dups != 1 {
		t.Fatalf("dups = %d", loss.Dups)
	}
	if r.Defects[0].Category != defect.Duplicate || r.Defects[0].Seq != 1 {
		t.Fatalf("defect = %+v", r.Defects[0])
	}
}

func TestReorderWithinWindow(t *testing.T) {
	r := NewReassembler(&LossTracker{})
	got := collect(t, r, 0, 2, 1, 3)
	if fmt.Sprint(got) != "[p0 p1 p2 p3]" {
		t.Fatalf("got %v", got)
	}
}

func TestLossBurstInterval(t *testing.T) {
	loss := &LossTracker{}
	r := NewReassembler(loss)
	var seqs []uint16
	for s := 0; s < 100; s++ {
		if s >= 40 && s < 50 {
			continue // burst of 10
		}
		seqs = append(seqs, uint16(s))
	}
	collect(t, r, seqs...)
	r.Finish()
	if loss.Missing() != 10 {
		t.Fatalf("missing = %d", loss.Missing())
	}
	if len(loss.Intervals) != 1 || loss.Intervals[0] != (Interval{Start: 40, End: 49}) {
		t.Fatalf("intervals = %+v", loss.Intervals)
	}
}

func TestLatePacketRecordedNotApplied(t *testing.T) {
	loss := &LossTracker{}
	r := NewReassembler(loss)
	// Push 0, then jump far ahead so the window slides past 5.
	collect(t, r, 0)
	collect(t, r, 2000)
	got := collect(t, r, 5) // already declared lost: late
	if len(got) != 0 {
		t.Fatalf("late packet contributed payload: %v", got)
	}
	if loss.Late != 1 {
		t.Fatalf("late = %d", loss.Late)
	}
	last := r.Defects[len(r.Defects)-1]
	if last.Category != defect.LatePacket || last.Seq != 5 {
		t.Fatalf("defect = %+v", last)
	}
}

func TestWraparound(t *testing.T) {
	r := NewReassembler(&LossTracker{})
	got := collect(t, r, 65534, 65535, 0, 1)
	want := "[p65534 p65535 p0 p1]"
	if fmt.Sprint(got) != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBadDatagram(t *testing.T) {
	r := NewReassembler(&LossTracker{})
	if _, err := r.Push([]byte{0x00, 0x01}); err == nil {
		t.Fatal("expected error for short datagram")
	}
}
