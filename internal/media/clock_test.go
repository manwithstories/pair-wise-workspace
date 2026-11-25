package media

import "testing"

func TestRationalNormalize(t *testing.T) {
	r := NewRational(2, 4)
	if r.Num != 1 || r.Den != 2 {
		t.Fatalf("got %v", r)
	}
	r = NewRational(3, -6)
	if r.Num != -1 || r.Den != 2 {
		t.Fatalf("sign normalize: got %v", r)
	}
	if (NewRational(5, 0) != Rational{0, 1}) {
		t.Fatal("zero den must yield 0/1")
	}
}

func TestRationalArithmetic(t *testing.T) {
	a := FromTicks(90000, 90000) // 1s
	b := FromTicks(1, 2)         // 0.5s
	if got := a.Add(b); got != NewRational(3, 2) {
		t.Fatalf("add: %v", got)
	}
	if got := a.Sub(b); got != NewRational(1, 2) {
		t.Fatalf("sub: %v", got)
	}
	if a.Cmp(b) <= 0 || b.Cmp(a) >= 0 || a.Cmp(a) != 0 {
		t.Fatal("cmp")
	}
	if got := a.Millis(); got != 1000 {
		t.Fatalf("millis: %v", got)
	}
	// Cross-timescale equality: 44100/44100 == 90000/90000.
	if FromTicks(44100, 44100).Cmp(FromTicks(90000, 90000)) != 0 {
		t.Fatal("cross-timescale cmp")
	}
}

func TestTicksRoundTrip(t *testing.T) {
	r := FromTicks(12345, 90000)
	if got := r.Ticks(90000); got != 12345 {
		t.Fatalf("round trip: %d", got)
	}
	// Non-zero start preserved exactly.
	start := FromTicks(1<<31+123, 90000)
	if got := start.Ticks(90000); got != 1<<31+123 {
		t.Fatalf("non-zero start: %d", got)
	}
}

func TestUnwrap32(t *testing.T) {
	prev := uint64(1<<32 - 10)
	got := Unwrap32(5, prev) // wrapped past 2^32
	if got != 1<<32+5 {
		t.Fatalf("forward wrap: %d", got)
	}
	// Late packet from previous cycle.
	got = Unwrap32(1<<32-20, 1<<32+5)
	if got != 1<<32-20 {
		t.Fatalf("backward: %d", got)
	}
}

func TestUnwrap16(t *testing.T) {
	prev := uint64(65535)
	if got := Unwrap16(0, prev); got != 65536 {
		t.Fatalf("seq wrap: %d", got)
	}
	if got := Unwrap16(65530, 65536+3); got != 65530 {
		t.Fatalf("late dup: %d", got)
	}
}

func TestErrorList(t *testing.T) {
	var l List
	l.Addf(SeverityRecoverable, "v1", "gap", Rational{}, Rational{1, 1}, "gap of %d ticks", 3)
	l.Addf(SeverityFatal, "a1", "truncated", Rational{}, Rational{}, "mdat cut")
	if l.Len() != 2 || !l.HasFatal() {
		t.Fatalf("list: %d fatal=%v", l.Len(), l.HasFatal())
	}
	if l.Count(SeverityRecoverable) != 1 {
		t.Fatal("count")
	}
}
