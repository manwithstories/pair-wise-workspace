package clock

import (
	"math/big"
	"testing"
)

func TestRationalNormalize(t *testing.T) {
	r := NewRational(2, 4)
	if r.Num != 1 || r.Den != 2 {
		t.Fatalf("2/4 normalised to %d/%d", r.Num, r.Den)
	}
	r = NewRational(3, -6)
	if r.Num != -1 || r.Den != 2 {
		t.Fatalf("3/-6 normalised to %d/%d", r.Num, r.Den)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("zero denominator did not panic")
		}
	}()
	NewRational(1, 0)
}

func TestUnwrap32Boundary(t *testing.T) {
	u := NewUnwrapper(32)
	const m = uint64(1) << 32
	raw := []uint64{m - 3, m - 2, m - 1, 0, 1, 2}
	var got []int64
	for _, v := range raw {
		got = append(got, u.Extend(v))
	}
	for i, want := range []int64{int64(m) - 3, int64(m) - 2, int64(m) - 1, int64(m), int64(m) + 1, int64(m) + 2} {
		if got[i] != want {
			t.Fatalf("extend[%d]=%d want %d", i, got[i], want)
		}
	}
}

func TestUnwrap16Sequence(t *testing.T) {
	u := NewUnwrapper(16)
	raw := []uint64{65534, 65535, 0, 1, 2}
	want := []int64{65534, 65535, 65536, 65537, 65538}
	for i, v := range raw {
		if got := u.Extend(v); got != want[i] {
			t.Fatalf("extend(%d)=%d want %d", v, got, want[i])
		}
	}
}

func TestUnwrapBackwardJitter(t *testing.T) {
	u := NewUnwrapper(32)
	u.Extend(100)
	if got := u.Extend(99); got != 99 {
		t.Fatalf("small backward step = %d, want 99", got)
	}
	if got := u.Extend(102); got != 102 {
		t.Fatalf("resume forward = %d, want 102", got)
	}
}

func driftAnchors(n int, stepLocal, nsPerTick, ppm int64) []Anchor {
	// ref = local * nsPerTick * (1 + ppm/1e6), kept exact by choosing
	// nsPerTick*(1e6+ppm) divisible by 1e6 for these test values.
	var out []Anchor
	for i := 0; i < n; i++ {
		l := int64(i) * stepLocal
		ref := l * nsPerTick * (1_000_000 + ppm) / 1_000_000
		out = append(out, Anchor{Local: l, Ref: ref})
	}
	return out
}

func TestFitExactNoDrift(t *testing.T) {
	f := Fitter{Nominal: NewRational(1, 1000), MinAnchors: 3, MaxDriftPPM: 500}
	fit := f.Fit(driftAnchors(10, 1000, 1_000_000, 0))
	if !fit.Aligned {
		t.Fatalf("not aligned: %s", fit.Reason)
	}
	if fit.DriftPPM.Sign() != 0 {
		t.Fatalf("drift = %s, want 0", fit.DriftPPM)
	}
	if got := MapTime(fit, 12345); got != 12345_000_000 {
		t.Fatalf("MapTime = %d, want %d", got, int64(12345_000_000))
	}
}

func TestFitDriftWithinThreshold(t *testing.T) {
	f := Fitter{Nominal: NewRational(1, 1000), MinAnchors: 3, MaxDriftPPM: 500}
	fit := f.Fit(driftAnchors(20, 500, 1_000_000, 100))
	if !fit.Aligned {
		t.Fatalf("not aligned: %s", fit.Reason)
	}
	// 100 ppm of 1e6 ns/tick = 100 ns/tick extra.
	want := big.NewRat(1_000_100, 1)
	if fit.Slope.Cmp(want) != 0 {
		t.Fatalf("slope = %s, want %s", fit.Slope, want)
	}
	if got := MapTime(fit, 5000); got != 5000*1_000_100 {
		t.Fatalf("MapTime = %d, want %d", got, 5000*1_000_100)
	}
}

func TestFitTooFewAnchors(t *testing.T) {
	f := Fitter{Nominal: NewRational(1, 1000), MinAnchors: 3, MaxDriftPPM: 500}
	fit := f.Fit(driftAnchors(2, 1000, 1_000_000, 0))
	if fit.Aligned {
		t.Fatal("aligned with too few anchors")
	}
	if fit.Reason != "insufficient anchors" {
		t.Fatalf("reason = %q", fit.Reason)
	}
	// Unaligned tracks keep nominal time: slope is the nominal timebase.
	if fit.Slope.Cmp(big.NewRat(1_000_000, 1)) != 0 {
		t.Fatalf("nominal slope = %s", fit.Slope)
	}
}

func TestFitExcessiveDrift(t *testing.T) {
	f := Fitter{Nominal: NewRational(1, 1000), MinAnchors: 3, MaxDriftPPM: 500}
	fit := f.Fit(driftAnchors(20, 500, 1_000_000, 5000))
	if fit.Aligned {
		t.Fatal("aligned despite excessive drift")
	}
	if fit.Reason != "drift exceeds threshold" {
		t.Fatalf("reason = %q", fit.Reason)
	}
}

func TestMapTimeRounding(t *testing.T) {
	fit := Fit{Slope: big.NewRat(1, 3), Intercept: new(big.Rat)}
	cases := []struct{ in, want int64 }{
		{0, 0}, {1, 0}, {2, 1}, {3, 1}, {4, 1}, {5, 2}, {-2, -1},
	}
	for _, c := range cases {
		if got := MapTime(fit, c.in); got != c.want {
			t.Fatalf("MapTime(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
