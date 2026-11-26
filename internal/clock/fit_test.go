package clock

import (
	"math"
	"testing"

	"mediad/internal/media"
)

func TestFitZeroDrift(t *testing.T) {
	tb := media.MustParseRational("1/48000")
	anchors := []Anchor{
		{HostUS: 1_700_000_000_000_000, MediaTicks: 96000},
		{HostUS: 1_700_000_005_000_000, MediaTicks: 96000 + 5*48000},
	}
	fit, err := FitModel(anchors, tb)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(fit.DriftPPM) > 1e-3 {
		t.Fatalf("drift=%f", fit.DriftPPM)
	}
}

func TestFitKnownDrift(t *testing.T) {
	tb := media.MustParseRational("1/48000")
	base := int64(1_700_000_000_000_000)
	start := int64(8589838592)
	// Camera runs 150 ppm slow: host elapsed is slightly more than media.
	hostElapsed := int64(7_000_000 + 7_000_000*150/1_000_000)
	fit, err := FitModel([]Anchor{
		{HostUS: base, MediaTicks: start},
		{HostUS: base + hostElapsed, MediaTicks: start + 7*48000},
	}, tb)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(fit.DriftPPM+150) > 2 {
		t.Fatalf("drift=%f want ~-150", fit.DriftPPM)
	}
	if fit.Anomaly {
		t.Fatal("150ppm should not be anomalous")
	}
}

func TestFitAnomalous(t *testing.T) {
	tb := media.MustParseRational("1/48000")
	base := int64(1_700_000_000_000_000)
	start := int64(1000)
	hostElapsed := int64(5_000_000 + 5_000_000*900/1_000_000) // 900 ppm
	fit, err := FitModel([]Anchor{
		{HostUS: base, MediaTicks: start},
		{HostUS: base + hostElapsed, MediaTicks: start + 5*48000},
	}, tb)
	if err != nil {
		t.Fatal(err)
	}
	if !fit.Anomaly {
		t.Fatalf("expected anomaly, drift=%f", fit.DriftPPM)
	}
	if fit.AppliedSlope() != 1 {
		t.Fatalf("anomalous fit must not correct: %f", fit.AppliedSlope())
	}
}
