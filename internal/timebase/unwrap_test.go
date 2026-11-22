package timebase

import (
	"testing"

	"github.com/pion/rtp"

	"mediarun/internal/mediaerr"
)

func Test16BitWraparoundExpandsMonotonic(t *testing.T) {
	u := NewUnwrapper(16)
	// Feed 0xFFFE, 0xFFFF, 0x0000, 0x0001: the wrap must yield 65534..65537.
	raw := []uint64{0xFFFE, 0xFFFF, 0x0000, 0x0001}
	want := []int64{65534, 65535, 65536, 65537}
	for i, r := range raw {
		got, err := u.Observe(r)
		if err != nil {
			t.Fatalf("Observe(%#x) unexpected error: %v", r, err)
		}
		if got != want[i] {
			t.Fatalf("Observe(%#x) = %d, want %d", r, got, want[i])
		}
	}
	if last, ok := u.Last(); !ok || last != 65537 {
		t.Fatalf("Last = (%d,%v), want 65537,true", last, ok)
	}
}

func Test32BitTimestampWraparound(t *testing.T) {
	u := NewUnwrapper(32)
	base := uint64(0xFFFFFFFF)
	first, err := u.Observe(base)
	if err != nil || first != int64(base) {
		t.Fatalf("origin = %d,%v", first, err)
	}
	got, err := u.Observe(0x00000002) // wrap + 3 ticks
	if err != nil {
		t.Fatalf("wrap step rejected: %v", err)
	}
	if want := int64(base) + 3; got != want {
		t.Fatalf("unwrapped = %d, want %d", got, want)
	}
}

func TestForwardGapRejectedWithPreciseOffset(t *testing.T) {
	u := NewUnwrapper(16)
	if _, err := u.Observe(100); err != nil {
		t.Fatal(err)
	}
	// Jump of 500 far exceeds the 100-step ceiling: ambiguous, must error.
	_, err := u.Observe(600)
	if err == nil {
		t.Fatal("expected SEQUENCE_GAP for large forward jump")
	}
	if !mediaerr.Is(err, mediaerr.CodeSequenceGap) {
		t.Fatalf("want SEQUENCE_GAP, got %v", err)
	}
	se := err.(*mediaerr.Error)
	if se.Offset != 1 { // zero-based: second observed value
		t.Fatalf("offset = %d, want 1", se.Offset)
	}
	// Rejected observation must not advance the unwrapper state.
	if n := u.Observed(); n != 1 {
		t.Fatalf("Observed = %d after rejected sample, want 1", n)
	}
	// Feeding the legitimate next value still works from the old origin.
	got, err := u.Observe(101)
	if err != nil || got != 101 {
		t.Fatalf("recovery Observe = %d,%v", got, err)
	}
}

func TestCustomJumpCeiling(t *testing.T) {
	u := NewUnwrapper(16).WithMaxJump(5)
	if _, err := u.Observe(0); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Observe(6); err == nil {
		t.Fatal("jump of 6 must exceed custom ceiling 5")
	}
	u2 := NewUnwrapper(16).WithMaxJump(5)
	_, _ = u2.Observe(0)
	if got, err := u2.Observe(5); err != nil || got != 5 {
		t.Fatalf("boundary step = %d,%v, want 5,nil", got, err)
	}
}

func TestTrackUnwrapperRTPWraparound(t *testing.T) {
	tr := NewTrackUnwrapper()
	// Two packets whose seq and ts both wrap the modular boundary.
	pkts := []*rtp.Packet{
		{Header: rtp.Header{SequenceNumber: 0xFFFF, Timestamp: 0xFFFFFFF0}},
		{Header: rtp.Header{SequenceNumber: 0x0000, Timestamp: 0x00000010}}, // +0x20 ticks across wrap
	}
	s0, ts0, err := tr.Observe(pkts[0])
	if err != nil {
		t.Fatal(err)
	}
	if s0 != 0xFFFF || ts0 != 0xFFFFFFF0 {
		t.Fatalf("first packet = seq %d ts %d", s0, ts0)
	}
	s1, ts1, err := tr.Observe(pkts[1])
	if err != nil {
		t.Fatalf("wrapped packet rejected: %v", err)
	}
	if s1 != 0x10000 {
		t.Fatalf("unwrapped seq = %d, want 65536", s1)
	}
	if ts1 != int64(0xFFFFFFF0)+0x20 {
		t.Fatalf("unwrapped ts = %d", ts1)
	}
	if tr.SeqObserved() != 2 {
		t.Fatalf("SeqObserved = %d, want 2", tr.SeqObserved())
	}
}

func TestTrackUnwrapperRejectsGapPacket(t *testing.T) {
	tr := NewTrackUnwrapper()
	_, _, _ = tr.Observe(&rtp.Packet{Header: rtp.Header{SequenceNumber: 10, Timestamp: 1000}})
	_, _, err := tr.Observe(&rtp.Packet{Header: rtp.Header{SequenceNumber: 5000, Timestamp: 2000}})
	if !mediaerr.Is(err, mediaerr.CodeSequenceGap) {
		t.Fatalf("want SEQUENCE_GAP, got %v", err)
	}
	if tr.SeqObserved() != 1 {
		t.Fatal("rejected packet must not be counted")
	}
}

func TestUnwrapperBitWidthGuard(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for unsupported bit width")
		}
	}()
	NewUnwrapper(8)
}
