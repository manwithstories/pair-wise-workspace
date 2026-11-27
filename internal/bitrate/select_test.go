package bitrate

import (
	"testing"

	"archive/internal/model"
)

func TestEstimatorSharpDropAndThrottle(t *testing.T) {
	log := model.NewFaultLog()
	e := NewEstimator(1800, log)
	e.Observe(2000)
	// A sample below 60% of the current estimate is a sharp drop.
	drop, rise := e.Observe(900)
	if !drop || rise {
		t.Fatal("900 vs ~2000 must classify as sharp drop")
	}
	if !e.Throttled() {
		t.Fatal("throttled flag must latch after sharp drop")
	}
	if log.Count(model.FaultBackpressure) != 1 {
		t.Fatal("drop event not recorded")
	}
	// Recovery below the 115% safety margin keeps throttling active.
	e.Observe(1500)
	if !e.Throttled() {
		t.Fatal("throttle must persist until strong recovery")
	}
	for range 4 {
		e.Observe(2400)
	}
	if e.Throttled() {
		t.Fatal("throttle must clear after sustained recovery above 115%")
	}
}

func TestEstimatorRiseNeedsPersistence(t *testing.T) {
	e := NewEstimator(1800, model.NewFaultLog())
	e.Observe(1500)
	// One isolated spike must not count as a persistent rise.
	drop, rise := e.Observe(3000)
	if drop || rise {
		t.Fatal("single spike must not be a persistent rise")
	}
	persistent := 0
	for range 4 {
		_, r := e.Observe(3200)
		if r {
			persistent++
		}
	}
	if persistent == 0 {
		t.Fatal("repeated high samples must eventually signal persistent rise")
	}
}

func TestSelectorDowngradeImmediateUpgradeHysteresis(t *testing.T) {
	log := model.NewFaultLog()
	reps := []Representation{
		{Name: "low", Kbps: 600},
		{Name: "mid", Kbps: 1800},
		{Name: "high", Kbps: 3600},
	}
	estim := NewEstimator(1800, log)
	sel := NewSelector(reps, estim, log)
	if sel.Current().Name != "mid" {
		t.Fatalf("initial selection = %s, want mid", sel.Current().Name)
	}

	// Sharp drop downgrades immediately, one step.
	drop, _ := estim.Observe(800)
	got := sel.Choose(drop, false)
	if got.Name != "low" {
		t.Fatalf("after drop selection = %s, want low", got.Name)
	}

	// A quick bounce back must not immediately re-upgrade: hysteresis.
	for i := 0; i < 2; i++ {
		_, rise := estim.Observe(2000)
		if sel.Choose(false, rise).Name != "low" {
			t.Fatal("transient recovery must not flip representation")
		}
	}
	// Sustained bandwidth clearly above 115% of mid (>=2070) upgrades.
	upgraded := false
	for i := 0; i < 6; i++ {
		_, rise := estim.Observe(2600)
		if sel.Choose(false, rise).Name == "mid" {
			upgraded = true
			break
		}
	}
	if !upgraded {
		t.Fatal("sustained high bandwidth never upgraded past hysteresis")
	}
}

func TestSelectorNoFlappingOnBursts(t *testing.T) {
	log := model.NewFaultLog()
	reps := []Representation{{Name: "low", Kbps: 500}, {Name: "mid", Kbps: 1500}}
	estim := NewEstimator(1500, log)
	sel := NewSelector(reps, estim, log)
	// Alternating burst/collapse samples: downgrade may happen, but the
	// representation must never oscillate back on a single good sample.
	samples := []float64{1500, 400, 2600, 400, 2600, 400}
	changes := 0
	prev := sel.Current().Name
	for _, s := range samples {
		d, r := estim.Observe(s)
		cur := sel.Choose(d, r).Name
		if cur != prev {
			changes++
			prev = cur
		}
	}
	if changes > 2 {
		t.Fatalf("representation flapped %d times on bursty input", changes)
	}
}

func TestBufferTargetReactsToPressure(t *testing.T) {
	e := NewEstimator(1800, model.NewFaultLog())
	base := model.Nano(250) * model.Millisecond
	e.Observe(2000)
	if got := e.BufferTargetNanos(base); got != base {
		t.Fatalf("comfortable bandwidth target = %v, want %v", got, base)
	}
	e.Observe(800) // sharp drop -> throttle
	if got := e.BufferTargetNanos(base); got <= base {
		t.Fatalf("throttled target must deepen buffer, got %v", got)
	}
}
