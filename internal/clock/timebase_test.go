package clock

import "testing"

func TestRationalNormalize(t *testing.T) {
	r := NewRational(4, -6)
	if r.Num != -2 || r.Den != 3 {
		t.Fatalf("normalize sign/reduce: got %d/%d", r.Num, r.Den)
	}
	if z := NewRational(0, 7); z != (Rational{0, 1}) {
		t.Fatalf("zero: got %+v", z)
	}
}

func TestRationalAddSubExact(t *testing.T) {
	// 1001/30000 + 1/90000 must stay exact; float64 would drift.
	a := NewRational(1001, 30000)
	b := NewRational(1, 90000)
	sum := a.Add(b)
	if want := NewRational(3004, 90000); sum != want {
		t.Fatalf("add: got %d/%d want %d/%d", sum.Num, sum.Den, want.Num, want.Den)
	}
	if d := sum.Sub(a); d != b {
		t.Fatalf("sub roundtrip: got %+v", d)
	}
}

func TestToTimebaseNoDrift(t *testing.T) {
	// 30000/1001 fps frame 1001 -> exactly 3003*90000/1001... use exact check.
	r := ToTimebase(1001, 30000) // 1001 ticks of a 30000/1001-scale
	if got := r.Ticks(); got != 3003 {
		t.Fatalf("ticks: got %d want 3003", got)
	}
	// AAC 1024/48000 frames: 1024*90000/48000 = 1920 exactly.
	if got := ToTimebase(1024, 48000).Ticks(); got != 1920 {
		t.Fatalf("aac frame: got %d want 1920", got)
	}
}

func TestTicksRounding(t *testing.T) {
	if got := NewRational(1, 2).Ticks(); got != 1 {
		t.Fatalf("half away from zero: got %d", got)
	}
	if got := NewRational(-1, 2).Ticks(); got != -1 {
		t.Fatalf("negative half: got %d", got)
	}
	if got := NewRational(3, 2).Ticks(); got != 2 {
		t.Fatalf("1.5 -> 2: got %d", got)
	}
}

func TestUnwrap33(t *testing.T) {
	const mod = 1 << 33
	prev := int64(mod - 5)
	got := Unwrap33(prev, 3) // wrapped 5->...->3 across the modulus
	if got != mod+3 {
		t.Fatalf("unwrap across wrap: got %d want %d", got, mod+3)
	}
	// No wrap: nearest continuation is the raw value itself.
	if got := Unwrap33(100, 105); got != 105 {
		t.Fatalf("no wrap: got %d", got)
	}
	// Backwards jump larger than half the modulus unwraps forward.
	if got := Unwrap33(mod+10, 20); got != mod+20 {
		t.Fatalf("second lap: got %d", got)
	}
}

func TestUnwrap32(t *testing.T) {
	const mod = 1 << 32
	if got := Unwrap(mod-2, 5, 32); got != mod+5 {
		t.Fatalf("rtp 32-bit wrap: got %d", got)
	}
}
