package media

import "testing"

func TestRationalScaleAndMilliseconds(t *testing.T) {
	base := Rational{Num: 1, Den: 90000}
	if got := Scale(90000, base, Rational{Num: 1, Den: 1000}); got != 1000 {
		t.Fatalf("scale = %d, want 1000", got)
	}
	if got := Ms(45000, base); got != 500 {
		t.Fatalf("Ms = %d, want 500", got)
	}
	if err := ValidateRational(base); err != nil {
		t.Fatal(err)
	}
}

func TestWrapExtenderExpandsForwardEpoch(t *testing.T) {
	w := NewPTSWrapExtender()
	const mod = 1 << 33
	first := w.Extend(mod - 3000)
	second := w.Extend(1500) // modulo value equivalent to mod+1500
	if second <= first {
		t.Fatalf("wrapped value not monotone: %d -> %d", first, second)
	}
	if w.Wraps != 1 {
		t.Fatalf("wraps = %d, want 1", w.Wraps)
	}
	if second-first != 4500 {
		t.Fatalf("delta = %d, want 4500", second-first)
	}
}

func TestDriftEstimatorIsBoundedAndCorrects(t *testing.T) {
	d := NewDriftEstimator(4)
	for i := int64(0); i < 10; i++ {
		d.Observe(i*1000, i*1002) // reference runs 0.2% fast relative to local slope
	}
	if len(d.samples) != 4 {
		t.Fatalf("window size = %d, want bounded 4", len(d.samples))
	}
	if ppm := d.DriftPPM(); ppm <= 1500 || ppm >= 2500 {
		t.Fatalf("drift ppm = %f, want about 2000", ppm)
	}
	if got := d.Correct(1_000_000); got <= 997_000 || got >= 999_000 {
		t.Fatalf("corrected = %d, want rate-adjusted value", got)
	}
}
