package clock

import (
	"errors"
	"math"
	"testing"
)

func TestRationalNormalizes(t *testing.T) {
	r := MustRational(60, -120)
	if r.Num != -1 || r.Den != 2 {
		t.Fatalf("got %s", r)
	}
	if got := MustRational(0, -7); got != (Rational{Num: 0, Den: 1}) {
		t.Fatalf("zero did not normalize to 0/1: %+v", got)
	}
	if _, err := NewRational(1, 0); !errors.Is(err, ErrZeroDenominator) {
		t.Fatalf("want zero denominator, got %v", err)
	}
	if _, err := NewRational(1, math.MinInt64); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want overflow for MinInt64 denominator, got %v", err)
	}
}

func TestRescaleRoundingModes(t *testing.T) {
	// One tick on a 3 Hz axis is 1/3 s, i.e. 1/3 of a tick on a 1 Hz axis.
	from := MustRational(3, 1)
	to := MustRational(1, 1)
	cases := []struct {
		mode Rounding
		want int64
	}{
		{RoundNearest, 0}, // 0.333 -> 0
		{RoundDown, 0},
		{RoundUp, 1},
		{RoundTowardZero, 0},
	}
	for _, c := range cases {
		got, err := Rescale(1, from, to, c.mode)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s: got %d want %d", c.mode, got, c.want)
		}
	}
	// 2/3 -> nearest 1, and negatives mirror the same policies.
	if got, _ := Rescale(2, from, to, RoundNearest); got != 1 {
		t.Errorf("2/3 nearest=%d", got)
	}
	if got, _ := Rescale(-2, from, to, RoundNearest); got != -1 {
		t.Errorf("-2/3 nearest=%d", got)
	}
	if got, _ := Rescale(-2, from, to, RoundDown); got != -1 {
		t.Errorf("-2/3 down=%d", got)
	}
	if got, _ := Rescale(-2, from, to, RoundUp); got != 0 {
		t.Errorf("-2/3 up=%d", got)
	}
}

func TestRescaleOverflow(t *testing.T) {
	// Scaling MaxInt64 up by 1000 ticks/s cannot fit in int64.
	_, err := Rescale(math.MaxInt64, MustRational(1, 1), MustRational(1000, 1), RoundNearest)
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("want overflow, got %v", err)
	}
	// The identity conversion is exact and must not overflow.
	if got, err := Rescale(math.MaxInt64, MustRational(1, 1), MustRational(1, 1), RoundNearest); err != nil || got != math.MaxInt64 {
		t.Fatalf("identity conversion=%d,%v", got, err)
	}
}
