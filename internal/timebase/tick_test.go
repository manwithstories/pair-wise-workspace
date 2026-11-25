package timebase_test

import (
	"testing"

	"bcast/internal/timebase"
)

func TestRationalArithmeticIsExact(t *testing.T) {
	a := timebase.MustNew(1, 3)
	b := timebase.MustNew(1, 6)
	got := timebase.Add(a, b)
	want := timebase.Rational{N: 1, D: 2}
	if timebase.Cmp(got, want) != 0 {
		t.Fatalf("1/3+1/6=%d/%d want 1/2", got.N, got.D)
	}
	if timebase.Cmp(timebase.Sub(a, b), b) != 0 {
		t.Fatal("1/3-1/6 must equal 1/6")
	}
}

func TestFrameBoundaryAndRescale(t *testing.T) {
	frame := timebase.Rational{N: 1, D: 8}
	at := timebase.MustNew(5, 8)
	n, err := timebase.FrameBoundary(at, frame)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("frame=%d want 5", n)
	}
	if got := timebase.Rescale(8000, 8000, 48000); got != 48000 {
		t.Fatalf("rescale=%d want 48000", got)
	}
	pts := timebase.SampleTime(6, 8)
	if pts.N != 3 || pts.D != 4 {
		t.Fatalf("pts=%d/%d want 3/4", pts.N, pts.D)
	}
}
