package clock

import (
	"errors"
	"math"
	"testing"
)

func TestRoundDiv(t *testing.T) {
	cases := []struct{ num, den, want int64 }{
		{0, 1, 0},
		{1, 2, 1},
		{3, 2, 2},
		{-1, 2, -1},
		{-3, 2, -2},
		{7, 3, 2},
		{-7, 3, -2},
		{8, 3, 3},
	}
	for _, c := range cases {
		if got := RoundDiv(c.num, c.den); got != c.want {
			t.Errorf("RoundDiv(%d,%d)=%d want %d", c.num, c.den, got, c.want)
		}
	}
}

func TestNanosOverflow(t *testing.T) {
	tb := MustTimebase(1_000_000)
	if _, err := tb.Nanos(math.MaxInt64); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want overflow, got %v", err)
	}
	got, err := tb.Nanos(1500)
	if err != nil || got != 1_500_000_000 {
		t.Fatalf("Nanos(1500)=%d,%v", got, err)
	}
}
