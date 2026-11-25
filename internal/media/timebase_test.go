package media

import "testing"

func TestRescaleExactRates(t *testing.T) {
	cases := []struct {
		v    int64
		from int64
		to   int64
		want int64
	}{
		{90000, 90000, 1000, 1000},
		{1000, 1000, 90000, 90000},
		{3000, 30000, 90000, 9000},
		{1, 1, 3, 3},
		{2, 3, 1, 1}, // 0.666 -> 1 (half away from zero)
		{-2, 3, 1, -1},
		{8589900000, 90000, 90000, 8589900000},
	}
	for _, c := range cases {
		got, err := Rescale(c.v, c.from, c.to)
		if err != nil {
			t.Fatalf("Rescale(%d,%d,%d): %v", c.v, c.from, c.to, err)
		}
		if got != c.want {
			t.Errorf("Rescale(%d,%d,%d)=%d want %d", c.v, c.from, c.to, got, c.want)
		}
	}
}

func TestUnwrap33(t *testing.T) {
	wrap := Wrap33
	cases := []struct {
		ref  int64
		raw  int64
		want int64
	}{
		// Forward wrap just past the 33-bit boundary.
		{wrap - 1000, int64(500), wrap + 500},
		// Backwards correction when the raw counter wrapped far behind.
		{wrap + 500, wrap - 1000, wrap - 1000},
		// No wrap.
		{123456, int64(123500), 123500},
		// Many wraps in: raw value is modulo 2^33.
		{5*wrap + 1234, int64(2000), 5*wrap + 2000},
	}
	for _, c := range cases {
		if got := UnwrapTimestamp(c.ref, c.raw); got != c.want {
			t.Errorf("UnwrapTimestamp(%d,%d)=%d want %d", c.ref, c.raw, got, c.want)
		}
	}
}

func TestRationalReduction(t *testing.T) {
	r, err := NewRational(60000, 2002)
	if err != nil {
		t.Fatal(err)
	}
	if r.Num != 30000 || r.Den != 1001 {
		t.Fatalf("got %d/%d", r.Num, r.Den)
	}
	inv, err := r.Inverse()
	if err != nil {
		t.Fatal(err)
	}
	dt, err := inv.RescaleTicks(90000)
	if err != nil {
		t.Fatal(err)
	}
	if dt != 3003 {
		t.Fatalf("frame duration ticks=%d want 3003", dt)
	}
}

func TestDriftEstimatorRejectsJump(t *testing.T) {
	var est DriftEstimator
	for i := 0; i < 100; i++ {
		local := int64(i) * 3003
		// 120 ppm slow camera: ref runs slightly behind local.
		ref := int64(float64(local) * (1 - 120e-6))
		if i == 50 {
			ref += 5_000_000 // a single gross jump
		}
		est.Add(ref, local)
	}
	fit, err := est.Fit()
	if err != nil {
		t.Fatal(err)
	}
	if fit.Outliers == 0 {
		t.Fatalf("expected the injected jump to be rejected: %+v", fit)
	}
	if fit.SlopePPM > -100 || fit.SlopePPM < -140 {
		t.Fatalf("slope %0.2f ppm outside expected drift", fit.SlopePPM)
	}
}
