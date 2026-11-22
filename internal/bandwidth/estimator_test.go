package bandwidth

import (
	"testing"

	"mediakernel/internal/clock"
)

func TestEstimatorSustainedRampMovesTarget(t *testing.T) {
	cfg := DefaultEstimatorConfig()
	e := NewEstimator(cfg)
	start := e.TargetBuffer()

	// Sustained bandwidth decline (same bytes over growing intervals) must
	// grow the target buffer after the confirmation run.
	arrival := clock.Ticks(0)
	for i := 0; i < 12; i++ {
		gap := clock.Milliseconds(10 + int64(i)*4)
		arrival += gap
		e.Observe(1000, arrival)
	}
	if e.BufferGrows() == 0 {
		t.Fatal("declining bandwidth never grew the target buffer")
	}
	if e.TargetBuffer() <= start || e.TargetBuffer() > cfg.MaxBuffer {
		t.Fatalf("target moved out of bounds: %v", e.TargetBuffer())
	}

	// Sustained bandwidth improvement must shrink it back toward the floor.
	e2 := NewEstimator(cfg)
	arrival = clock.Ticks(0)
	for i := 0; i < 12; i++ {
		gap := clock.Milliseconds(60 - int64(i)*3)
		arrival += gap
		e2.Observe(1000, arrival)
	}
	if e2.BufferShrinks() == 0 {
		t.Fatal("improving bandwidth never shrank the target buffer")
	}
	if e2.TargetBuffer() >= start || e2.TargetBuffer() < cfg.MinBuffer {
		t.Fatalf("target moved out of bounds: %v", e2.TargetBuffer())
	}
}

func TestEstimatorClampsAndNoise(t *testing.T) {
	cfg := DefaultEstimatorConfig()
	e := NewEstimator(cfg)
	arrival := clock.Ticks(0)
	// Alternating observations must not move the target before confirmation.
	for i := 0; i < 10; i++ {
		if i%2 == 0 {
			arrival += clock.Milliseconds(20)
		} else {
			arrival += clock.Milliseconds(12)
		}
		e.Observe(800, arrival)
	}
	mid := (cfg.MinBuffer + cfg.MaxBuffer) / 2
	if e.TargetBuffer() != mid {
		t.Fatalf("noisy observations moved target: %v want %v", e.TargetBuffer(), mid)
	}
	// Hammer the limits; target must stay clamped to its configured ceiling.
	// A low EWMA decay keeps consecutive observations above the 2% movement
	// threshold so every confirmation window genuinely re-evaluates the cap.
	clampCfg := EstimatorConfig{
		MinBuffer:           clock.Milliseconds(40),
		MaxBuffer:           clock.Milliseconds(260),
		EWMADecay:           0.2,
		ConfirmRuns:         2,
		StepTicks:           clock.Milliseconds(20),
		BytesPerSecondFloor: 1,
	}
	ce := NewEstimator(clampCfg)
	t0 := clock.Ticks(0)
	for i := 0; i < 60; i++ {
		t0 += clock.Milliseconds(10 + int64(i)*5)
		ce.Observe(1, t0)
	}
	if ce.TargetBuffer() != clampCfg.MaxBuffer {
		t.Fatalf("max clamp failed: %v want %v", ce.TargetBuffer(), clampCfg.MaxBuffer)
	}
}

func TestSameTickBurstDoesNotDivideByZero(t *testing.T) {
	e := NewEstimator(DefaultEstimatorConfig())
	e.Observe(500, clock.Milliseconds(10))
	e.Observe(500, clock.Milliseconds(10))
	if e.Observations() != 2 || e.EstimateBps() < 0 {
		t.Fatal("same-tick burst handling broken")
	}
}
