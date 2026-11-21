package transport

import (
	"math"
	"testing"
)

func TestShortSamplesDoNotDownshift(t *testing.T) {
	cfg := DefaultBandwidthConfig()
	cfg.DefaultTargetKbps = 1800
	b := NewBandwidthEstimator(cfg)
	// A single tiny sample must not move the target, even though it is far
	// below the default: the window is not warm.
	rec := b.Add(BandwidthSample{AtMillis: 0, DeliveryKbps: 120})
	if rec.Warm {
		t.Fatal("single sample marked warm")
	}
	if rec.TargetKbps != 1800 {
		t.Fatalf("short sample moved target to %.1f", rec.TargetKbps)
	}
	if math.Abs(rec.ErrorPct) <= 0 {
		t.Fatal("deviation should still be recorded for a short sample")
	}
}

func TestAdaptationFollowsChangingBandwidth(t *testing.T) {
	cfg := DefaultBandwidthConfig()
	cfg.DefaultTargetKbps = 1800
	b := NewBandwidthEstimator(cfg)
	// Sustained delivery below target must lower the target gradually.
	for i := 0; i < 6; i++ {
		b.Add(BandwidthSample{AtMillis: int64(i * 100), DeliveryKbps: 900})
	}
	low := b.Target()
	if low >= 1800 {
		t.Fatalf("target did not adapt down: %.1f", low)
	}
	// Sustained delivery above target must raise it again.
	for i := 6; i < 20; i++ {
		b.Add(BandwidthSample{AtMillis: int64(i * 100), DeliveryKbps: 2600})
	}
	if b.Target() <= low {
		t.Fatalf("target did not adapt up: %.1f -> %.1f", low, b.Target())
	}
}

func TestStepAndHysteresisBounds(t *testing.T) {
	cfg := BandwidthConfig{WindowSamples: 3, MinSamples: 1, DefaultTargetKbps: 1000,
		MaxStepPct: 10, HeadroomPct: 0, HysteresisPct: 20}
	b := NewBandwidthEstimator(cfg)
	// A massive jump should move at most 10% in one tick.
	rec := b.Add(BandwidthSample{DeliveryKbps: 5000})
	if rec.TargetKbps != 1100 {
		t.Fatalf("step not bounded: %.1f", rec.TargetKbps)
	}
	// A window average within 20% of the current target should not cause a
	// retarget; use a fresh estimator so the 5000 sample has slid out.
	h := NewBandwidthEstimator(BandwidthConfig{WindowSamples: 1, MinSamples: 1,
		DefaultTargetKbps: 1000, MaxStepPct: 10, HeadroomPct: 0, HysteresisPct: 20})
	if got := h.Add(BandwidthSample{DeliveryKbps: 1010}).TargetKbps; got != 1000 {
		t.Fatalf("hysteresis not applied: %.1f", got)
	}
}

func TestRecordsCarryTargetActualAndError(t *testing.T) {
	b := NewBandwidthEstimator(BandwidthConfig{WindowSamples: 3, MinSamples: 2, DefaultTargetKbps: 1000,
		MaxStepPct: 100, HeadroomPct: 0, HysteresisPct: 0})
	b.Add(BandwidthSample{AtMillis: 0, DeliveryKbps: 1000})
	b.Add(BandwidthSample{AtMillis: 100, DeliveryKbps: 800})
	recs := b.Records()
	if len(recs) != 2 {
		t.Fatalf("records = %d", len(recs))
	}
	if recs[1].TargetKbps != 900 || recs[1].ActualKbps != 800 {
		t.Fatalf("record = %+v", recs[1])
	}
	if math.Abs(recs[1].ErrorPct-12.5) > 1e-9 {
		t.Fatalf("error pct = %v, want 12.5", recs[1].ErrorPct)
	}
	sum := b.Summary()
	if sum.Records != 2 || sum.TargetKbps != 900 || sum.MeanActualKbps != 900 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestWindowSlides(t *testing.T) {
	b := NewBandwidthEstimator(BandwidthConfig{WindowSamples: 3, MinSamples: 3, DefaultTargetKbps: 1000,
		MaxStepPct: 100, HeadroomPct: 0, HysteresisPct: 0})
	b.Add(BandwidthSample{DeliveryKbps: 100})
	b.Add(BandwidthSample{DeliveryKbps: 100})
	b.Add(BandwidthSample{DeliveryKbps: 100})
	if got := b.Target(); got != 100 {
		t.Fatalf("target after low window = %.1f", got)
	}
	// Old samples must slide out. Steps are capped at 100% of the current
	// target, so convergence takes a few ticks but must reach the new level.
	for i := 0; i < 12; i++ {
		b.Add(BandwidthSample{DeliveryKbps: 2000})
	}
	if got := b.Target(); got != 2000 {
		t.Fatalf("window did not slide: %.1f", got)
	}
}
