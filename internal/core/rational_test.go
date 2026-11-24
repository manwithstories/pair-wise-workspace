package core

import (
	"math"
	"testing"
)

func TestScaleTicksExactLadderConversions(t *testing.T) {
	cases := []struct {
		v            int64
		from, to     uint32
		wantFloor    int64
	}{
		{48000, 48000, 90000, 90000},
		{9600, 48000, 90000, 18000},
		{6000, 30000, 90000, 18000},
		{18000, 90000, 90000, 18000},
	}
	for _, c := range cases {
		got, ok := ScaleTicksFloor(c.v, c.from, c.to)
		if !ok || got != c.wantFloor {
			t.Errorf("ScaleTicksFloor(%d,%d,%d)=%d,%v want %d", c.v, c.from, c.to, got, ok, c.wantFloor)
		}
	}
}

func TestScaleTicksRejectsZeroRate(t *testing.T) {
	if _, ok := ScaleTicksFloor(10, 0, 90000); ok {
		t.Fatal("zero from-rate must fail")
	}
}

func TestMulDivSignsAndRounding(t *testing.T) {
	if q, ok := MulDivFloor(9, 1, 4); !ok || q != 2 {
		t.Fatalf("floor 9/4=%d,%v want 2", q, ok)
	}
	if q, ok := MulDivRound(1, 1, 2); !ok || q != 1 {
		t.Fatalf("round .5=%d,%v want 1", q, ok)
	}
	if q, ok := MulDivFloor(-90000, 1, 1); !ok || q != -90000 {
		t.Fatalf("negative=%d,%v", q, ok)
	}
}

func TestMulDivOverflow(t *testing.T) {
	if _, ok := MulDivFloor(math.MaxInt64, 2, 1); ok {
		t.Fatal("MaxInt64*2/1 must report overflow")
	}
	if _, ok := ScaleTicksFloor(math.MaxInt64, 1, 1_000_000_000); ok {
		// MaxInt64 * 1e9 / 1 overflows the 128-bit quotient check path or
		// yields a value > MaxInt64; either way ok must be false.
		t.Fatal("huge scale must report overflow")
	}
}

func TestAddChecked(t *testing.T) {
	if _, ok := AddChecked(math.MaxInt64, 1); ok {
		t.Fatal("positive overflow not detected")
	}
	if _, ok := AddChecked(math.MinInt64, -1); ok {
		t.Fatal("negative overflow not detected")
	}
	if v, ok := AddChecked(10, 5); !ok || v != 15 {
		t.Fatalf("normal add=%d,%v", v, ok)
	}
}
