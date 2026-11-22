package rtp

import (
	"testing"

	"github.com/pion/rtp"

	"mediascan/internal/clock"
	"mediascan/internal/fault"
)

func newPkt(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: seq, Timestamp: ts, PayloadType: 96, Version: 2},
		Payload: []byte{byte(seq)},
	}
}

func testClock(t *testing.T) clock.Clock {
	t.Helper()
	clk, err := clock.New(90000, 30)
	if err != nil {
		t.Fatal(err)
	}
	return clk
}

func TestInOrderNoFaults(t *testing.T) {
	r, _ := NewReorderer(8, testClock(t))
	for i := uint16(0); i < 10; i++ {
		if err := r.Push(newPkt(i, uint32(i)*3000)); err != nil {
			t.Fatal(err)
		}
	}
	res := r.Finalize()
	if len(res.Ordered) != 10 || res.Duplicates != 0 || res.Reordered != 0 || len(res.Bursts) != 0 {
		t.Fatalf("clean trace produced anomalies: %+v", res)
	}
}

func TestExpandSeqWrapsAndReorders(t *testing.T) {
	r, _ := NewReorderer(32, testClock(t))
	r.Push(newPkt(0xFFFD, 0))
	r.Push(newPkt(0xFFFE, 0))
	r.Push(newPkt(0xFFFF, 0))
	// true wrap: 0 and 1 belong to the next 16-bit cycle
	if got := r.expandSeq(0); got != 0x10000 {
		t.Fatalf("wrap seq0 ext = %#x, want 0x10000", got)
	}
	if got := r.expandSeq(1); got != 0x10001 {
		t.Fatalf("wrap seq1 ext = %#x, want 0x10001", got)
	}
	// in-cycle reorder on the first cycle stays on the first cycle
	if got := r.expandSeq(0xFFFE); got != 0xFFFE {
		t.Fatalf("reorder ext = %#x, want 0xFFFE", got)
	}
	// ordinary small-number stream never wraps
	r2, _ := NewReorderer(8, testClock(t))
	r2.Push(newPkt(0, 0))
	r2.Push(newPkt(1, 0))
	r2.Push(newPkt(3, 0))
	if got := r2.expandSeq(2); got != 2 {
		t.Fatalf("in-cycle reorder ext = %d, want 2", got)
	}
}

func TestDuplicateAndReorder(t *testing.T) {
	r, _ := NewReorderer(8, testClock(t))
	// 0,1,2 then a duplicate 1, then 4 then the missing 3 (inside window)
	for _, seq := range []uint16{0, 1, 2, 1, 4, 3} {
		if err := r.Push(newPkt(seq, uint32(seq)*3000)); err != nil {
			t.Fatal(err)
		}
	}
	res := r.Finalize()
	if res.Duplicates != 1 {
		t.Fatalf("duplicates = %d, want 1", res.Duplicates)
	}
	if res.Reordered != 1 {
		t.Fatalf("reordered = %d, want 1", res.Reordered)
	}
	want := []uint16{0, 1, 2, 3, 4}
	if len(res.Ordered) != len(want) {
		t.Fatalf("ordered = %d packets, want %d", len(res.Ordered), len(want))
	}
	for i := range want {
		if res.Ordered[i].SequenceNumber != want[i] {
			t.Fatalf("ordered[%d] = %d, want %d", i, res.Ordered[i].SequenceNumber, want[i])
		}
	}
}

func TestBurstLossMerged(t *testing.T) {
	r, _ := NewReorderer(8, testClock(t))
	for _, seq := range []uint16{0, 1, 5, 6} { // 2,3,4 missing in one burst
		if err := r.Push(newPkt(seq, uint32(seq)*3000)); err != nil {
			t.Fatal(err)
		}
	}
	res := r.Finalize()
	if len(res.Bursts) != 1 || res.Bursts[0].Count != 3 {
		t.Fatalf("bursts = %+v", res.Bursts)
	}
	if len(res.MissingSeq) != 3 {
		t.Fatalf("missing = %v", res.MissingSeq)
	}
	if !hasCat(res.Faults, fault.CatBurstLoss) {
		t.Fatalf("burst fault missing: %v", res.Faults)
	}
}

func TestLateOutsideWindow(t *testing.T) {
	r, _ := NewReorderer(4, testClock(t))
	// seq 2 is never sent; 0,1 then 3..10 slide the window well past it
	for seq := uint16(0); seq <= 10; seq++ {
		if seq == 2 {
			continue
		}
		if err := r.Push(newPkt(seq, uint32(seq)*3000)); err != nil {
			t.Fatal(err)
		}
	}
	// seq 2 now arrives after the contiguous head advanced beyond it
	if err := r.Push(newPkt(2, 6000)); err != nil {
		t.Fatal(err)
	}
	res := r.Finalize()
	if res.Late != 1 {
		t.Fatalf("late = %d, want 1", res.Late)
	}
	if !hasCat(res.Faults, fault.CatRTPLate) {
		t.Fatalf("late fault missing: %v", res.Faults)
	}
}

func TestSequenceWrapDuplicateReorder(t *testing.T) {
	// 0xFFFD..0xFFFF arrive, then 0xFFFE is re-sent before the wrap
	// (duplicate near the boundary); wrapped 1 then arrives before 0 so
	// one packet is reordered across the 16-bit wrap
	r, _ := NewReorderer(32, testClock(t))
	for _, seq := range []uint16{0xFFFD, 0xFFFE, 0xFFFF, 0xFFFE, 1, 0} {
		if err := r.Push(newPkt(seq, clock.Wrap32(int64(seq)*3000))); err != nil {
			t.Fatal(err)
		}
	}
	res := r.Finalize()
	if res.Duplicates != 1 {
		t.Fatalf("wrap duplicates = %d, want 1", res.Duplicates)
	}
	if res.Reordered != 1 {
		t.Fatalf("wrap reordered = %d, want 1", res.Reordered)
	}
	wantExt := []uint64{0xFFFD, 0xFFFE, 0xFFFF, 0x10000, 0x10001}
	if len(res.Ordered) != len(wantExt) {
		t.Fatalf("ordered len = %d, want %d", len(res.Ordered), len(wantExt))
	}
	base := res.MinExtSeq
	for i, want := range wantExt {
		if base+uint64(i) != want {
			t.Fatalf("position %d ext mismatch: %d vs %d", i, base+uint64(i), want)
		}
	}
}

func TestTimestampWrapExpand(t *testing.T) {
	clk := testClock(t)
	r, _ := NewReorderer(8, clk)
	r.Push(newPkt(0, 0xFFFFFFF0))
	r.Push(newPkt(1, 0x00000008)) // 0x100000008 after expansion
	res := r.Finalize()
	if len(res.Ordered) != 2 {
		t.Fatalf("ordered = %d", len(res.Ordered))
	}
	if d := clock.Delta32(0xFFFFFFF0, res.Ordered[1].Timestamp); d != 24 {
		t.Fatalf("wrapped timestamp delta = %d, want 24", d)
	}
}

func TestWindowGuard(t *testing.T) {
	if _, err := NewReorderer(0, testClock(t)); err == nil {
		t.Fatal("zero window must be rejected")
	}
	if _, err := NewReorderer(MaxWindow+1, testClock(t)); err == nil {
		t.Fatal("oversized window must be rejected")
	}
}

func hasCat(fs []fault.Fault, cat fault.Category) bool {
	for _, f := range fs {
		if f.Cat == cat {
			return true
		}
	}
	return false
}
