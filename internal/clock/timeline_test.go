package clock

import (
	"errors"
	"math"
	"testing"
)

func TestRationalNormalisation(t *testing.T) {
	r, err := NewRational(500, 1000)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if r.Num != 1 || r.Den != 2 {
		t.Fatalf("want 1/2 got %s", r)
	}
	if _, err := NewRational(1, 0); err == nil {
		t.Fatal("want zero denominator error")
	}
	if _, err := NewRational(-1, 2); err == nil {
		t.Fatal("want negative time error")
	}
}

func TestNonZeroOriginRoundTrip(t *testing.T) {
	// 90 kHz video clock starting at a non-zero PTS of 1.234 s.
	tb, err := NewTimebase(90000, 111060)
	if err != nil {
		t.Fatal(err)
	}
	for _, ticks := range []int64{0, 1, 3003, 90000, 90000*60 + 7} {
		abs, err := tb.At(ticks)
		if err != nil {
			t.Fatalf("At(%d): %v", ticks, err)
		}
		back, err := tb.ToTicks(abs)
		if err != nil {
			t.Fatalf("ToTicks(%s): %v", abs, err)
		}
		if back != ticks {
			t.Fatalf("tick round trip: want %d got %d", ticks, back)
		}
		errMs, err := tb.RoundTripErrorMillis(abs)
		if err != nil {
			t.Fatal(err)
		}
		if errMs > 1.0 {
			t.Fatalf("round trip error %f ms exceeds 1 ms", errMs)
		}
	}
}

func TestToTicksRoundsHalfUp(t *testing.T) {
	tb, err := NewTimebase(1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 1.5 ms at 1 kHz -> 1.5 ticks -> rounds to 2.
	abs, _ := NewRational(3, 2000)
	ticks, err := tb.ToTicks(abs)
	if err != nil {
		t.Fatal(err)
	}
	if ticks != 2 {
		t.Fatalf("want 2 got %d", ticks)
	}
}

func TestOverflowReported(t *testing.T) {
	r := Rational{Num: math.MaxInt64 / 2, Den: 1}
	if _, err := r.MulInt(4); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want overflow got %v", err)
	}
	tb, _ := NewTimebase(4, 0)
	huge := Rational{Num: math.MaxInt64 / 2, Den: 1}
	if _, err := tb.ToTicks(huge); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want overflow got %v", err)
	}
}

func TestSubRejectsNegativeDuration(t *testing.T) {
	a, _ := NewRational(1, 10)
	b, _ := NewRational(2, 10)
	if _, err := a.Sub(b); err == nil {
		t.Fatal("want negative duration error")
	}
}

func TestToTicksClampsBeforeOrigin(t *testing.T) {
	tb, _ := NewTimebase(1000, 500)
	before, _ := NewRational(1, 10) // 100 ms, origin is 500 ms
	ticks, err := tb.ToTicks(before)
	if err != nil {
		t.Fatal(err)
	}
	if ticks != 0 {
		t.Fatalf("want 0 before origin, got %d", ticks)
	}
}
