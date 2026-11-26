package clock

import (
	"math/big"
	"testing"
)

func TestMulDivExact(t *testing.T) {
	cases := []struct{ a, b, c, want int64 }{
		{1001, 90000, 30000, 3003},          // NTSC frame -> 90 kHz
		{1000, 90000, 48000, 1875},       // 1000 audio ticks @48k
		{0, 90000, 90000, 0},                // origin
		{-1001, 90000, 30000, -3003},        // negative media time
		{10800 * 90000, 90000, 90000, 10800 * 90000}, // 3 h identity
		{7, 1, 2, 3},                        // truncation toward zero
		{-7, 1, 2, -3},
	}
	for _, tc := range cases {
		if got := MulDiv(tc.a, tc.b, tc.c); got != tc.want {
			t.Errorf("MulDiv(%d,%d,%d)=%d want %d", tc.a, tc.b, tc.c, got, tc.want)
		}
	}
}

// Cross-check MulDiv against math/big on pseudo-random values.
func TestMulDivAgainstBigInt(t *testing.T) {
	seed := uint64(0x9e3779b97f4a7c15)
	next := func(mod int64) int64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		v := int64(seed>>11) % mod // keep a*b/c within int64 domain
		if seed&1 == 0 {
			return -v
		}
		return v
	}
	for i := 0; i < 20000; i++ {
		a, b := next(1<<40), next(1<<20)
		c := next(1<<20)%(1<<20-1) + 1
		if c < 0 {
			c = -c
		}
		got := MulDiv(a, b, c)
		want := new(big.Int).Quo(
			new(big.Int).Mul(big.NewInt(a), big.NewInt(b)), big.NewInt(c)).Int64()
		if got != want {
			t.Fatalf("MulDiv(%d,%d,%d)=%d want %d", a, b, c, got, want)
		}
	}
}

// Converting absolute positions must never drift, unlike float accumulation.
func TestTimelineNoAccumulatedDrift(t *testing.T) {
	tl := Timeline{Timescale: 30000}
	const delta = 1001
	prev := int64(-1)
	for n := int64(1); n <= 100000; n++ {
		got := tl.ToClock(n * delta)
		want := MulDiv(n*delta, Rate, 30000)
		if got != want {
			t.Fatalf("n=%d got %d want %d", n, got, want)
		}
		if got < prev {
			t.Fatalf("n=%d clock went backwards: %d < %d", n, got, prev)
		}
		prev = got
	}
}

func TestTimelineRoundTrip(t *testing.T) {
	for _, ts := range []uint32{90000, 48000, 44100, 30000, 25000} {
		tl := Timeline{Timescale: ts}
		if !tl.Valid() {
			t.Fatal("timeline should be valid")
		}
		for sec := int64(0); sec < 10800; sec += 997 {
			clk := tl.ToClock(sec * int64(ts))
			if clk != sec*Rate {
				t.Fatalf("ts=%d sec=%d clock=%d want %d", ts, sec, clk, sec*Rate)
			}
			if back := tl.FromClock(clk); back != sec*int64(ts) {
				t.Fatalf("ts=%d roundtrip=%d want %d", ts, back, sec*int64(ts))
			}
		}
	}
	if (Timeline{}).Valid() {
		t.Fatal("zero timescale must be invalid")
	}
}
