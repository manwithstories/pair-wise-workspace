package bandwidth

import (
	"math"
	"testing"
)

func TestNewValidates(t *testing.T) {
	if _, err := New(0, 0.3); err == nil {
		t.Fatal("zero target should fail")
	}
	if _, err := New(1000, 0); err == nil {
		t.Fatal("zero alpha should fail")
	}
	if _, err := New(1000, 1.5); err == nil {
		t.Fatal("alpha > 1 should fail")
	}
}

func TestObserveUpdatesAverage(t *testing.T) {
	e, err := New(2000, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	// 1 MB over 4 seconds = 2000 kbps.
	e.ObserveBlock(1_000_000, 4)
	if math.Abs(e.AverageKbps()-2000) > 0.001 {
		t.Fatalf("average = %v", e.AverageKbps())
	}
	if math.Abs(e.DeviationPct()) > 0.001 {
		t.Fatalf("deviation = %v", e.DeviationPct())
	}
}

func TestDeviationSignAndMagnitude(t *testing.T) {
	e, err := New(2000, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	// 900,000 bytes over 4s = 1800 kbps, -10%.
	e.ObserveBlock(900_000, 4)
	if math.Abs(e.DeviationPct()+10) > 0.001 {
		t.Fatalf("deviation = %v", e.DeviationPct())
	}
	if !e.Finite() {
		t.Fatal("estimate should be finite")
	}
}

func TestSmoothingMovesGradually(t *testing.T) {
	e, _ := New(2000, 0.25)
	e.ObserveBlock(1_000_000, 4) // 2000 kbps
	e.ObserveBlock(2_000_000, 4) // 4000 kbps
	// EMA: 0.25*4000 + 0.75*2000 = 2500.
	if math.Abs(e.Current-2500) > 0.001 {
		t.Fatalf("ema = %v", e.Current)
	}
}

func TestPlanBlockSamples(t *testing.T) {
	e, _ := New(2000, 0.5)
	e.ObserveBlock(1_000_000, 4) // 250000 bytes/s
	if n := e.PlanBlockSamples(100, 2500); n != 100 {
		t.Fatalf("plan = %d, want capped 100", n)
	}
	if n := e.PlanBlockSamples(1000, 250_000); n != 1 {
		t.Fatalf("plan = %d, want 1", n)
	}
	if n := e.PlanBlockSamples(0, 100); n != 0 {
		t.Fatalf("plan = %d, want 0", n)
	}
	if n := e.PlanBlockSamples(10, 0); n != 10 {
		t.Fatalf("plan = %d, want 10", n)
	}
}

func TestIgnoresInvalidObservations(t *testing.T) {
	e, _ := New(2000, 0.5)
	e.ObserveBlock(-1, 1)
	e.ObserveBlock(100, 0)
	if e.Blocks() != 0 {
		t.Fatalf("blocks = %d", e.Blocks())
	}
}
