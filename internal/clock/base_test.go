package clock

import (
	"math"
	"testing"
)

func TestRationalArithmeticAndCompare(t *testing.T) {
	a := New(1, 3)
	b := New(2, 6) // normalizes to 1/3
	if a != b {
		t.Fatalf("normalization mismatch: %v vs %v", a, b)
	}
	if a.Cmp(New(1, 2)) >= 0 {
		t.Fatal("1/3 must be < 1/2")
	}
	got := New(1, 2).Add(New(1, 3))
	if got != New(5, 6) {
		t.Fatalf("1/2+1/3=%v want 5/6", got)
	}
	if d := MillisDiff(New(1, 1000), Zero()); math.Abs(d-1.0) > 1e-9 {
		t.Fatalf("1/1000s = %v ms want 1", d)
	}
}

func TestRescaleExactAVTimescales(t *testing.T) {
	// One 90 kHz tick and 48 kHz ticks must both land exactly on the
	// shared 720000 tick grid.
	v, ok := RescaleExact(90000, 90000, CommonTimescale) // 1 second
	if !ok || v != 720000 {
		t.Fatalf("video rescale = %d,%v", v, ok)
	}
	a, ok := RescaleExact(48000, 48000, CommonTimescale)
	if !ok || a != 720000 {
		t.Fatalf("audio rescale = %d,%v", a, ok)
	}
	// 44100 does not divide 720000 integrally for every tick.
	if _, ok := RescaleExact(1, 44100, CommonTimescale); ok {
		t.Fatal("1/44100 must not rescale exactly onto 720000 grid")
	}
	r := RescaleRound(22050, 44100, 48000) // 0.5s -> 24000
	if r != 24000 {
		t.Fatalf("rounded rescale = %d want 24000", r)
	}
}

func TestTimebaseMillis(t *testing.T) {
	tb := Timebase{Timescale: 90000}
	if m := tb.Millis(3000); math.Abs(m-33.333333333333336) > 1e-6 {
		t.Fatalf("3000/90000 = %v ms", m)
	}
}
