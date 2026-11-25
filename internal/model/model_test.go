package model

import (
	"errors"
	"testing"
)

func TestRationalReduceAndEqual(t *testing.T) {
	a, err := NewRational(48000, 1)
	if err != nil {
		t.Fatal(err)
	}
	b := MustRational(96000, 2)
	if !a.Equal(b) {
		t.Fatalf("expected reduced equality: %v vs %v", a, b)
	}
	if _, err := NewRational(1, 0); err == nil {
		t.Fatal("expected zero denominator error")
	}
}

func TestRescale(t *testing.T) {
	src := Hz(48000)
	dst := Hz(90000) // RTP clock
	// 48000 audio frames (1s) -> 90000 RTP ticks.
	if got := src.Rescale(48000, dst); got != 90000 {
		t.Fatalf("rescale 1s = %d, want 90000", got)
	}
	// Round half up: 1 frame at 48k expressed in 90k = 1.875 -> 2.
	if got := src.Rescale(1, dst); got != 2 {
		t.Fatalf("rescale 1 frame = %d, want 2", got)
	}
}

func TestNanosRoundTrip(t *testing.T) {
	r := Hz(48000)
	for _, f := range []int64{0, 1, 48000, 480000, 1234567} {
		ns := r.Nanos(f)
		back := r.TicksFromNanos(ns)
		if back != f {
			t.Fatalf("round trip frame %d -> ns %d -> frame %d", f, ns, back)
		}
	}
}

func TestFaultSentinelAndSet(t *testing.T) {
	err := NewFault(KindCRC, "a", FrameRange{First: 1, Last: 2}, 10, "bad")
	if !errors.Is(err, ErrInvalid) {
		t.Fatal("CRC fault must anchor to ErrInvalid")
	}
	idx := NewFault(KindIndex, "i", FrameRange{}, -1, "x")
	if !errors.Is(idx, ErrIndexUnrecoverable) {
		t.Fatal("index fault must anchor to ErrIndexUnrecoverable")
	}
	s := NewFaultSet()
	s.AddErr(err)
	s.AddErr(err) // duplicate
	s.Add(idx.Fault)
	if s.Count() != 2 {
		t.Fatalf("count = %d, want 2", s.Count())
	}
	if !s.Has(KindCRC) || !s.Has(KindIndex) {
		t.Fatal("missing expected kinds")
	}
}
