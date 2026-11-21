package clock

import (
	"errors"
	"testing"
)

func TestRationalReduce(t *testing.T) {
	r, err := NewRational(60000, 2002)
	if err != nil {
		t.Fatal(err)
	}
	if r.Num != 30000 || r.Den != 1001 {
		t.Fatalf("got %v want 30000/1001", r)
	}
	r, err = NewRational(4, -8)
	if err != nil {
		t.Fatal(err)
	}
	if r.Num != -1 || r.Den != 2 {
		t.Fatalf("got %v want -1/2", r)
	}
	if _, err := NewRational(1, 0); err == nil {
		t.Fatal("expected zero denominator error")
	}
	if r.String() != "-1/2" {
		t.Fatalf("string %q", r.String())
	}
	if MustRational(6, 3).String() != "2" {
		t.Fatalf("integer formatting")
	}
}

func TestRationalArithmetic(t *testing.T) {
	a := MustRational(1, 3)
	b := MustRational(1, 6)
	if got := a.Add(b); got != MustRational(1, 2) {
		t.Fatalf("add %v", got)
	}
	if got := a.Sub(b); got != MustRational(1, 6) {
		t.Fatalf("sub %v", got)
	}
	if got := a.Mul(b); got != MustRational(1, 18) {
		t.Fatalf("mul %v", got)
	}
	if a.Cmp(b) <= 0 || b.Cmp(a) >= 0 || a.Cmp(a) != 0 {
		t.Fatal("cmp")
	}
	if (Span{Start: a, End: a.Add(b)}).Duration() != b {
		t.Fatal("span duration")
	}
}

func TestTimebaseDurations(t *testing.T) {
	tb := Must(30000, 1001)
	if d := tb.Duration(30000); d != MustRational(1001, 1) {
		t.Fatalf("duration %v want 1001", d)
	}
	if d := tb.Duration(1001); d != MustRational(1002001, 30000) {
		t.Fatalf("duration %v want 1002001/30000", d)
	}
	sp := tb.Span(100, 200)
	if sp.End.Cmp(sp.Start) <= 0 {
		t.Fatalf("bad span %v", sp)
	}
	if _, err := New(0, 1); err == nil {
		t.Fatal("accept zero rate")
	}
	if _, err := New(48000, -1); err == nil {
		t.Fatal("accept negative denominator")
	}
}

func TestRescaleExact(t *testing.T) {
	a := Must(48000, 1)
	b := Must(96000, 1)
	got, err := RescaleExact(480, a, b)
	if err != nil || got != 960 {
		t.Fatalf("got %d err %v", got, err)
	}
	// 1 tick at 30000/1001 -> 1001/30000 s; not an integer at 48000 Hz
	ntsc := Must(30000, 1001)
	if _, err := RescaleExact(1, ntsc, a); err == nil {
		t.Fatal("expected inexact error")
	} else {
		var de *DivisibleError
		if !errors.As(err, &de) {
			t.Fatalf("wrong error type %T", err)
		}
	}
	// A 29.97 frame is 1601.6 samples at 48 kHz; five frames are exact.
	if got, err := RescaleExact(5, ntsc, a); err != nil {
		t.Fatal(err)
	} else if got != 8008 {
		t.Fatalf("got %d want 8008", got)
	}
}

func TestRescaleNearest(t *testing.T) {
	a := Must(48000, 1)
	ntsc := Must(30000, 1001)
	// one NTSC tick is 33.3667ms -> 1601.6 samples, rounds to 1602
	if got := RescaleNearest(1, ntsc, a); got != 1602 {
		t.Fatalf("got %d want 1602", got)
	}
	if got := RescaleNearest(0, ntsc, a); got != 0 {
		t.Fatalf("zero got %d", got)
	}
}

func TestCheckMonotonic(t *testing.T) {
	if err := CheckMonotonic([]int64{1, 2, 4000}); err != nil {
		t.Fatal(err)
	}
	err := CheckMonotonic([]int64{1, 2, 2, 3})
	var oe *OrderError
	if !errors.As(err, &oe) {
		t.Fatalf("want OrderError got %v", err)
	}
	if oe.Index != 2 || oe.Prev != 2 || oe.Cur != 2 {
		t.Fatalf("bad fields %+v", oe)
	}
	if CheckMonotonic([]int64{5, 4}) == nil {
		t.Fatal("expected descending error")
	}
}
