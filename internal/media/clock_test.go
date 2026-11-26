package media

import (
	"errors"
	"testing"
)

func TestClockRoundTrip(t *testing.T) {
	c := Clock{TrackID: 1, Timescale: 48000}
	for _, in := range []uint64{0, 1, 2400, 48000, 1234567} {
		g, ok := c.ToGlobal(in)
		if !ok {
			t.Fatalf("ToGlobal overflow for %d", in)
		}
		back, ok := c.FromGlobal(g)
		if !ok {
			t.Fatalf("FromGlobal overflow for %d", g)
		}
		// Rounding may differ by at most one track unit.
		if diff := int64(in) - int64(back); diff > 1 || diff < -1 {
			t.Fatalf("round trip drift: in=%d back=%d", in, back)
		}
	}
}

func TestClockOverflow(t *testing.T) {
	c := Clock{TrackID: 1, Timescale: 1}
	if _, ok := c.ToGlobal(MaxTime); ok {
		t.Fatal("expected overflow on huge track time")
	}
	if _, ok := (Clock{TrackID: 1, Timescale: 48000}).FromGlobal(MaxTime); ok {
		t.Fatal("expected overflow on huge global time")
	}
	if _, ok := (Clock{Timescale: 0}).ToGlobal(10); ok {
		t.Fatal("zero timescale must fail")
	}
}

func TestAddTime(t *testing.T) {
	if v, ok := AddTime(MaxTime-2, 2); !ok || v != MaxTime {
		t.Fatalf("edge sum wrong: v=%d ok=%v", v, ok)
	}
	if _, ok := AddTime(MaxTime, 1); ok {
		t.Fatal("expected overflow")
	}
}

func TestFaultIsAndIsolation(t *testing.T) {
	f := NewFault(FaultTruncated, 7, 3, 128, "moof runs past EOF")
	if !errors.Is(f, &Fault{Code: FaultTruncated}) {
		t.Fatal("errors.Is should match by code")
	}
	if errors.Is(f, &Fault{Code: FaultConflict}) {
		t.Fatal("different codes must not match")
	}
	if !f.Isolation() {
		t.Fatal("truncation must isolate")
	}
	gap := NewFault(FaultSequenceGap, 1, 4, 0, "missing sequence")
	if gap.Isolation() {
		t.Fatal("repairable gap must not isolate")
	}
}
