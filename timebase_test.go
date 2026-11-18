package main

import (
	"math"
	"math/big"
	"testing"
)

func TestTimebaseValid(t *testing.T) {
	cases := []struct {
		tb   Timebase
		want bool
	}{
		{Timebase{1, 1_000_000_000}, true},
		{Timebase{1, 768000}, true},
		{Timebase{0, 1000}, false},
		{Timebase{1, 0}, false},
		{Timebase{-1, 1000}, false},
		{Timebase{1, -1000}, false},
	}
	for _, c := range cases {
		if got := c.tb.Valid(); got != c.want {
			t.Errorf("Timebase{%d,%d}.Valid()=%v want %v", c.tb.Num, c.tb.Den, got, c.want)
		}
	}
}

func TestTicksToNs(t *testing.T) {
	// 1µs 时间基：1 tick = 1000ns
	got, err := ticksToNs(40_000, 1, 1_000_000)
	if err != nil || got != 40_000_000 {
		t.Fatalf("µs ticks: got %d err %v, want 40000000", got, err)
	}
	// 音频时间基 1/768000：7680 ticks = 10ms = 1e7ns
	got, err = ticksToNs(7680, 1, 768_000)
	if err != nil || got != 10_000_000 {
		t.Fatalf("audio ticks: got %d err %v, want 10000000", got, err)
	}
	// 零值
	got, _ = ticksToNs(0, 1, 90000)
	if got != 0 {
		t.Fatalf("zero ticks: got %d want 0", got)
	}
}

func TestRatToInt64EvenHalfToEven(t *testing.T) {
	// 覆盖正/负、半偶取舍与非半值；参考实现 math.RoundToEven 对这些小
	// 有理数精确，可作为权威口径。
	fracs := [][2]int64{
		{5, 2}, {7, 2}, {-5, 2}, {-7, 2},
		{1, 3}, {2, 3}, {-1, 3}, {-2, 3},
		{9, 4}, {11, 4}, {-9, 4}, {-11, 4},
		{4, 2}, {-4, 2}, {0, 1},
	}
	for i, f := range fracs {
		r := big.NewRat(f[0], f[1])
		got, err := ratToInt64Even(r)
		if err != nil {
			t.Fatalf("case %d unexpected err %v", i, err)
		}
		exp := int64(math.RoundToEven(float64(f[0]) / float64(f[1])))
		if got != exp {
			t.Errorf("case %d %d/%d: got %d want %d", i, f[0], f[1], got, exp)
		}
	}
}

func TestRatToInt64EvenOverflow(t *testing.T) {
	// MaxInt64 + 1/2 应溢出而非静默回绕。
	r := new(big.Rat).SetInt64(math.MaxInt64)
	r.Add(r, big.NewRat(1, 2))
	if _, err := ratToInt64Even(r); err != ErrOverflow {
		t.Fatalf("positive overflow: err=%v want ErrOverflow", err)
	}
	// MinInt64 - 1 必然落在可表示范围之外。
	r2 := new(big.Rat).SetInt64(math.MinInt64)
	r2.Sub(r2, big.NewRat(1, 1))
	if _, err := ratToInt64Even(r2); err != ErrOverflow {
		t.Fatalf("negative overflow: err=%v want ErrOverflow", err)
	}
}

func TestSecondsToTargetTicksRoundTrip(t *testing.T) {
	// 任意时间基 -> 纳秒 -> 同一时间基，舍入应自洽。
	for _, tb := range []Timebase{{1, 1_000_000}, {1, 768000}, {1, 90000}} {
		for _, ticks := range []int64{0, 1, 7680, 1234567} {
			sec := ticksToRat(ticks, tb.Num, tb.Den)
			back, err := secondsToTargetTicks(sec, tb.Num, tb.Den)
			if err != nil {
				t.Fatalf("tb=%v ticks=%d roundtrip err %v", tb, ticks, err)
			}
			if back != ticks {
				t.Errorf("tb=%v ticks=%d roundtrip=%d", tb, ticks, back)
			}
		}
	}
}
