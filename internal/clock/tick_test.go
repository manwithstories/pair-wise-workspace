package clock

import "testing"

func TestRatNormalize(t *testing.T) {
	r := New(4, -8)
	if r.N != -1 || r.D != 2 {
		t.Fatalf("New(4,-8) = %v, want -1/2", r)
	}
	if z := New(0, 5); z.N != 0 || z.D != 1 {
		t.Fatalf("zero normalize = %v", z)
	}
}

func TestRatArithmetic(t *testing.T) {
	a := New(1, 3)
	b := New(1, 6)
	if got := a.Add(b); got != New(1, 2) {
		t.Fatalf("1/3+1/6 = %v", got)
	}
	if got := a.Sub(b); got != New(1, 6) {
		t.Fatalf("1/3-1/6 = %v", got)
	}
	if got := a.Mul(b); got != New(1, 18) {
		t.Fatalf("1/3*1/6 = %v", got)
	}
}

func TestRatCmpExact(t *testing.T) {
	// Large denominators that would overflow int64 if cross-multiplied
	// naively in 64 bits after repeated additions.
	a := New(1<<40, (1<<32)*750)
	b := New((1<<40)+1, (1<<32)*750)
	if a.Cmp(b) != -1 || b.Cmp(a) != 1 || a.Cmp(a) != 0 {
		t.Fatalf("Cmp failed for large rationals")
	}
	if New(-3, 4).Cmp(New(1, 8)) != -1 {
		t.Fatalf("sign Cmp failed")
	}
	if New(-1, 2).Cmp(New(-1, 3)) != -1 {
		t.Fatalf("negative Cmp failed")
	}
}

func TestCalibrationToShared(t *testing.T) {
	c := NewCalibration(48000, 1000, New(10, 1))
	// ts = anchor -> anchor NTP
	if got := c.ToShared(1000); got != New(10, 1) {
		t.Fatalf("anchor maps to %v, want 10/1", got)
	}
	// +48000 ticks = +1s
	if got := c.ToShared(49000); got != New(11, 1) {
		t.Fatalf("+1s maps to %v, want 11/1", got)
	}
	// wrap: anchor near 2^32, ts just past wrap
	cw := NewCalibration(48000, 0xFFFFF000, New(5, 1))
	var base uint32 = 0xFFFFF000
	wrapped := base + 48000 // wraps to 0xBB80
	got := cw.ToShared(wrapped)
	want := New(6, 1)
	if got != want {
		t.Fatalf("wrapped ts maps to %v, want %v", got, want)
	}
}

func TestMediaClockTicks(t *testing.T) {
	mc := MediaClock{Rate: 48000}
	if got := mc.Ticks(960); got != New(1, 50) {
		t.Fatalf("960 ticks @48k = %v, want 1/50", got)
	}
}
