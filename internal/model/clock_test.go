package model

import (
	"errors"
	"fmt"
	"testing"
)

func TestTimestampConversionsRoundTrip(t *testing.T) {
	cases := []struct {
		units     uint64
		timescale uint32
	}{
		{0, 90000},
		{1, 90000},
		{3003, 90000}, // 29.97 fps duration
		{44100, 44100},
		{1024, 48000},
		{1 << 32, 90000},
	}
	for _, c := range cases {
		ns := ToNanoseconds(c.units, c.timescale)
		back := FromNanoseconds(ns, c.timescale)
		// allow at most one timescale unit of rounding error
		d := int64(back) - int64(c.units)
		if d < 0 {
			d = -d
		}
		if d > 1 {
			t.Fatalf("round trip units=%d ts=%d got %d (ns=%d)", c.units, c.timescale, back, ns)
		}
	}
}

func TestToNanosecondsZeroTimescale(t *testing.T) {
	if got := ToNanoseconds(100, 0); got != 0 {
		t.Fatalf("zero timescale must yield 0, got %d", got)
	}
	if got := FromNanoseconds(100, 0); got != 0 {
		t.Fatalf("zero timescale must yield 0, got %d", got)
	}
}

func TestDriftMs(t *testing.T) {
	if got := DriftMs(15*Millisecond, 0); got != 15 {
		t.Fatalf("drift = %d, want 15", got)
	}
	// 1500us = 1.5ms truncates toward zero
	if got := DriftMs(0, 1500*Microsecond); got != 1 {
		t.Fatalf("drift = %d, want 1 (truncated)", got)
	}
}

func TestUnwrap32BitRTP(t *testing.T) {
	const mod = uint64(1) << 32
	// continuous sequence
	if got := Unwrap(1000, 1001, mod); got != 1001 {
		t.Fatalf("continuous unwrap = %d", got)
	}
	// forward wrap near the 32-bit boundary
	prev := uint64(mod - 10)
	cur := uint64(5)
	if got := Unwrap(prev, cur, mod); got != mod+5 {
		t.Fatalf("wraparound unwrap = %d, want %d", got, mod+5)
	}
	// zero modulus is a no-op
	if got := Unwrap(40, 42, 0); got != 42 {
		t.Fatalf("zero modulus unwrap = %d", got)
	}
}

func TestFirstRegression(t *testing.T) {
	if i := FirstRegression([]Timestamp{1, 2, 3, 3, 4}); i != -1 {
		t.Fatalf("monotonic sequence reported regression at %d", i)
	}
	if i := FirstRegression([]Timestamp{1, 5, 4, 6}); i != 2 {
		t.Fatalf("regression index = %d, want 2", i)
	}
	if i := FirstRegression(nil); i != -1 {
		t.Fatalf("empty sequence reported regression at %d", i)
	}
}

func TestFaultWrappingAndOffset(t *testing.T) {
	base := NewFault(FaultOutOfBounds, 4096, "cam1.mp4", "sample exceeds mdat")
	wrapped := fmt.Errorf("transport stage: %w", base)
	if !errors.Is(wrapped, base) {
		t.Fatal("errors.Is must find the wrapped Fault")
	}
	f, ok := AsFault(wrapped)
	if !ok || f.Offset != 4096 || f.Kind != FaultOutOfBounds {
		t.Fatalf("AsFault = %+v ok=%v", f, ok)
	}
	if got := FirstBadOffset(errors.New("plain")); got != OffsetUnknown {
		t.Fatalf("plain error offset = %d, want %d", got, OffsetUnknown)
	}
	if got := FirstBadOffset(wrapped); got != 4096 {
		t.Fatalf("FirstBadOffset = %d, want 4096", got)
	}
}
