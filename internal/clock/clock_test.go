package clock

import (
	"fmt"
	"testing"

	"mediad/internal/media"
)

// TestUnwrapCrosses33BitBoundary feeds a monotonic stream that crosses the
// 33-bit boundary and checks the unwrapped values continue without a jump.
func TestUnwrapCrosses33BitBoundary(t *testing.T) {
	const width = 33
	u, err := NewUnwrapper(width)
	if err != nil {
		t.Fatal(err)
	}
	period := int64(1) << width
	start := period - 5
	var prev int64
	got := make([]int64, 0, 20)
	for i := 0; i < 20; i++ {
		raw := (start + int64(i)*1000) % period
		v := u.Unwrap(raw)
		got = append(got, v)
		if i > 0 && v <= prev {
			t.Fatalf("non-monotonic unwrap at %d: %d <= %d", i, v, prev)
		}
		prev = v
	}
	if got[0] >= period {
		t.Fatalf("first value should be unadjusted, got %d", got[0])
	}
	if got[len(got)-1] <= period {
		t.Fatalf("crossing should lift later values above the period, got %d", got[len(got)-1])
	}
	if got[len(got)-1]-got[0] != 19*1000 {
		t.Fatalf("span mismatch: %d", got[len(got)-1]-got[0])
	}
	if u.Wraps() != 1 {
		t.Fatalf("wraps=%d", u.Wraps())
	}
}

// TestUnwrapKeepsSmallBackwardStepsInEpoch models ordinary out-of-order
// delivery: a little jitter must not be misread as a wrap.
func TestUnwrapKeepsSmallBackwardStepsInEpoch(t *testing.T) {
	u, _ := NewUnwrapper(33)
	u.Unwrap(100000)
	v := u.Unwrap(90000) // 10k ticks behind, far below half the period
	if v != 90000 {
		t.Fatalf("jitter mangled: %d", v)
	}
	if u.Wraps() != 0 {
		t.Fatalf("spurious wrap: %d", u.Wraps())
	}
}

// TestUnwrapNearChoosesNearestEpoch places a wrapped tick across the boundary
// using anchor references on either side.
func TestUnwrapNearChoosesNearestEpoch(t *testing.T) {
	const width = 33
	period := int64(1) << width
	refs := []int64{period - 1000, period + 3000}
	// Raw value 1000 belongs to the epoch after the boundary.
	got, err := UnwrapNear(1000, width, refs)
	if err != nil {
		t.Fatal(err)
	}
	if got != period+1000 {
		t.Fatalf("got %d want %d", got, period+1000)
	}
	// A value near period-2000 stays in the pre-boundary epoch.
	got, _ = UnwrapNear(period-2000, width, refs)
	if got != period-2000 {
		t.Fatalf("pre-boundary: %d", got)
	}
}

// TestUnwrapInvalidWidth rejects unusable wrap widths.
func TestUnwrapInvalidWidth(t *testing.T) {
	if _, err := NewUnwrapper(0); err == nil {
		t.Fatal("width 0 should fail")
	}
	if _, err := NewUnwrapper(63); err == nil {
		t.Fatal("width 63 should fail")
	}
}

// TestFitRecoversDriftAcrossWrap checks the fit is unaffected by the raw
// counter wrap because anchors are already unwrapped.
func TestFitRecoversDriftAcrossWrap(t *testing.T) {
	tb := media.MustParseRational("1/48000")
	period := int64(1) << 33
	start := period - 2*48000
	base := int64(1_700_000_000_000_000)
	// The camera records 10 media seconds in slightly less host time, so its
	// clock runs fast by 120 ppm.
	hostSpan := int64(10_000_000 - 10_000_000*120/1_000_000)
	fit, err := FitModel([]Anchor{
		{HostUS: base, MediaTicks: start},
		{HostUS: base + hostSpan, MediaTicks: start + 10*48000},
	}, tb)
	if err != nil {
		t.Fatal(err)
	}
	if fit.Anomaly {
		t.Fatal("120ppm is within tolerance")
	}
	if d := fit.DriftPPM - 120; d > 5 || d < -5 {
		t.Fatalf("drift=%.2f want ~+120", fit.DriftPPM)
	}
}

// TestAnomalyIsReportedNotCorrected asserts anomalous drift leaves the slope
// untouched so delivery is never silently warped.
func TestAnomalyIsReportedNotCorrected(t *testing.T) {
	tb := media.MustParseRational("1/48000")
	base := int64(1_700_000_000_000_000)
	hostSpan := int64(4_000_000 - 4_000_000*500/1_000_000) // 500 ppm fast
	fit, err := FitModel([]Anchor{
		{HostUS: base, MediaTicks: 0},
		{HostUS: base + hostSpan, MediaTicks: 4 * 48000},
	}, tb)
	if err != nil {
		t.Fatal(err)
	}
	if !fit.Anomaly || fit.AppliedSlope() != 1 {
		t.Fatalf("anomaly=%t slope=%.6f", fit.Anomaly, fit.AppliedSlope())
	}
	if fmt.Sprintf("%.0f", fit.DriftPPM) != "500" {
		t.Fatalf("drift=%.3f", fit.DriftPPM)
	}
}
