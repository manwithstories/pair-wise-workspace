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
