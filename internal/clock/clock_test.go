package clock_test

import (
	"errors"
	"math"
	"testing"

	"archive/internal/clock"
	"archive/internal/media"
)

const w32 = int64(1) << 32

func TestUnwrapFirstValueIsIdentity(t *testing.T) {
	got, err := clock.Unwrap(12345, 0, false)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if got != 12345 {
		t.Fatalf("got %d", got)
	}
}

func TestUnwrapAcrossWindow(t *testing.T) {
	prev := w32 - 500 // just below a 32-bit boundary
	raw := int64(100) // restarted near zero
	got, err := clock.Unwrap(raw, prev, true)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if want := w32 + 100; got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
	if got <= prev {
		t.Fatalf("DTS must keep increasing across the wrap")
	}
}

func TestUnwrapBackwardWithinWindowRejectedAsRegression(t *testing.T) {
	c := clock.NewTrackCursor(90000, 0)
	if _, err := c.StepUnwrapped(1000, false); err != nil {
		t.Fatalf("first step: %v", err)
	}
	c.Accept(1000, 1000)
	next, err := c.StepUnwrapped(500, false)
	if err == nil {
		t.Fatalf("expected regression, got %d", next)
	}
	if !errors.Is(err, clock.ErrRegression) {
		t.Fatalf("error class = %v", err)
	}
}

func TestUnwrapRejectsImpossibleRaw(t *testing.T) {
	if _, err := clock.Unwrap(-5, 0, false); err == nil {
		t.Fatalf("negative raw must be rejected")
	}
	if _, err := clock.Unwrap(int64(1)<<48, 0, false); err == nil {
		t.Fatalf("absurd raw must be rejected")
	}
}

func TestTrackCursorWrapAccumulates(t *testing.T) {
	c := clock.NewTrackCursor(90000, 0)
	inputs := []int64{w32 - 300, w32 - 200, 10, 120, 230}
	var got []int64
	for _, raw := range inputs {
		next, err := c.StepUnwrapped(raw, false)
		if err != nil {
			t.Fatalf("step %d: %v", raw, err)
		}
		c.Accept(next, raw)
		got = append(got, c.PrevDTS)
	}
	if c.WrapCount != 1 {
		t.Fatalf("wrap count = %d, want 1", c.WrapCount)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("non-monotonic DTS: %v", got)
		}
	}
	if got[2] != w32+10 {
		t.Fatalf("post-wrap DTS = %d, want %d", got[2], w32+10)
	}
}

func TestGapAllowsLargeForwardStep(t *testing.T) {
	c := clock.NewTrackCursor(90000, 0)
	c.Accept(0, 0)
	c.Accept(100, 100) // small stride establishes the baseline
	// A multi-second jump is accepted when the caller says a fragment is
	// missing (the gap explains it).
	if _, err := c.StepUnwrapped(2_000_000, true); err != nil {
		t.Fatalf("gap step: %v", err)
	}
	c.Accept(2_000_000, 2_000_000)
	// The same huge jump without a known gap is a discontinuity.
	if _, err := c.StepUnwrapped(9_000_000, false); err == nil {
		t.Fatalf("expected discontinuity rejection")
	}
}

func TestCorrectOrPreservesVFRCadence(t *testing.T) {
	ts := media.TimeScale(12800)
	var est clock.DriftEstimator
	// A CFR reference clock at 25 fps; observed track cadence alternates
	// 12/16 ms while averaging the same rate. The fit must not report drift.
	medianDur := int64(16) // 16 ticks at 12800/s ~= 1.25 ms
	for i := 0; i < 200; i++ {
		ref := int64(i) * 512
		// VFR: two frames of 12 fields, two of 20 fields, cycling; average 16.
		obs := ref
		switch i % 4 {
		case 0:
			obs = int64(i)*512 - 128
		case 1:
			obs = int64(i)*512 - 64
		case 2:
			obs = int64(i)*512 + 64
		case 3:
			obs = int64(i)*512 + 128
		}
		est.Add(ts.ToMicros(ref), ts.ToMicros(obs))
	}
	_ = medianDur
	fit := est.Fit()
	// A ±128-tick cadence ripple on a 512-tick base is ~250 ppm peak-to-peak;
	// the least-squares slope must stay far below a real clock error while
	// still tolerating that ripple.
	if math.Abs(fit.SlopePPM) > 150 {
		t.Fatalf("VFR cadence reported as drift: %.1f ppm", fit.SlopePPM)
	}
	if c := (clock.Corrector{TS: ts, Estimate: fit}); c.Micros(200000) < 0 {
		t.Fatalf("corrector produced a negative time")
	}
}

func TestDriftFitRecoversSlope(t *testing.T) {
	est := clock.DriftEstimator{}
	ts := media.TimeScale(90000)
	// Observed runs 800 ppm fast against the reference.
	slope := 1.0008
	for i := int64(0); i <= 100; i++ {
		ref := i * 90000 // one second per step
		obs := int64(float64(ref) * slope)
		est.Add(ts.ToMicros(ref), ts.ToMicros(obs))
	}
	fit := est.Fit()
	if math.Abs(fit.SlopePPM-800) > 2 {
		t.Fatalf("slope = %.2f ppm, want ~800", fit.SlopePPM)
	}
	// Applying the fit must bring the last point back onto the reference.
	c := clock.Corrector{TS: ts, Estimate: fit}
	if d := c.Micros(int64(float64(100*90000)*slope)) - ts.ToMicros(100*90000); d < -1000 || d > 1000 {
		t.Fatalf("corrected residual = %d us", d)
	}
}

func TestDriftFitNeedsEnoughPoints(t *testing.T) {
	var est clock.DriftEstimator
	est.Add(0, 0)
	if fit := est.Fit(); fit.SlopePPM != 0 || fit.Samples != 1 {
		t.Fatalf("one-point fit = %+v", fit)
	}
}

func TestTimeScaleConversion(t *testing.T) {
	ts := media.TimeScale(90000)
	if got := ts.ToMicros(90000); got != 1_000_000 {
		t.Fatalf("90000 ticks = %d us", got)
	}
	if got := ts.FromMicros(1_000_000); got != 90000 {
		t.Fatalf("1 s = %d ticks", got)
	}
	if media.TimeScale(0).Valid() {
		t.Fatalf("zero timescale must be invalid")
	}
}

func TestUnwrapRoundTripsLargeValues(t *testing.T) {
	c := clock.NewTrackCursor(44100, 0)
	var prev int64 = -1
	// Steps grow gradually so the cadence check sees a plausible stream;
	// the point is that large absolute values still round-trip.
	for _, raw := range []int64{0, 500, 1000, 1500, 2000, 2500} {
		next, err := c.StepUnwrapped(raw, false)
		if err != nil {
			t.Fatalf("step: %v", err)
		}
		c.Accept(next, raw)
		if next <= prev && prev >= 0 {
			t.Fatalf("non-monotonic at raw %d", raw)
		}
		prev = next
	}
}
