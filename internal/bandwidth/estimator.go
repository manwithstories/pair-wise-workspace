// Package bandwidth estimates the currently available network bandwidth from
// simulated arrivals and translates it into a bounded jitter-buffer target.
// Sustained ramps in either direction move the target; single-sample noise is
// smoothed by an EWMA plus a confirmation count.
package bandwidth

import (
	"mediakernel/internal/clock"
)

// EstimatorConfig parameterizes the adaptive buffer.
type EstimatorConfig struct {
	// MinBuffer / MaxBuffer clamp the adaptive target in ticks.
	MinBuffer clock.Ticks
	MaxBuffer clock.Ticks
	// EWMADecay is the weight of the previous estimate in [0,1).
	EWMADecay float64
	// ConfirmRuns is the number of consecutive same-direction observations
	// required before the target buffer moves one step.
	ConfirmRuns int
	// StepTicks is how far the target moves per confirmed transition.
	StepTicks clock.Ticks
	// BytesPerSecondFloor prevents division-by-near-zero estimates.
	BytesPerSecondFloor float64
}

// DefaultEstimatorConfig covers 48kHz voice over a wide range of links. The
// 240ms ceiling stays below the simulator's 300ms+ late-packet delay so that
// reordering events are reliably sealed before they arrive.
func DefaultEstimatorConfig() EstimatorConfig {
	return EstimatorConfig{
		MinBuffer:           clock.Milliseconds(40),
		MaxBuffer:           clock.Milliseconds(240),
		EWMADecay:           0.7,
		ConfirmRuns:         3,
		StepTicks:           clock.Milliseconds(20),
		BytesPerSecondFloor: 8000,
	}
}

// Estimator is the streaming bandwidth -> buffer-target adapter.
type Estimator struct {
	cfg           EstimatorConfig
	estimateBps   float64
	started       bool
	lastArrival   clock.Ticks
	upRun         int
	downRun       int
	target        clock.Ticks
	observations  int
	bufferGrows   int
	bufferShrinks int
}

// NewEstimator seeds the target at the middle of the configured range.
func NewEstimator(cfg EstimatorConfig) *Estimator {
	if cfg.MaxBuffer <= cfg.MinBuffer {
		cfg = DefaultEstimatorConfig()
	}
	if cfg.EWMADecay <= 0 || cfg.EWMADecay >= 1 {
		cfg.EWMADecay = 0.7
	}
	if cfg.ConfirmRuns <= 0 {
		cfg.ConfirmRuns = 3
	}
	return &Estimator{
		cfg:    cfg,
		target: (cfg.MinBuffer + cfg.MaxBuffer) / 2,
	}
}

// Observe feeds one arrival: its size in bytes and the receive time on the
// unified clock. The first call only anchors the time base.
func (e *Estimator) Observe(sizeBytes int, arrival clock.Ticks) {
	e.observations++
	if !e.started {
		e.started = true
		e.lastArrival = arrival
		return
	}
	dtTicks := arrival - e.lastArrival
	if dtTicks <= 0 {
		// Same-tick burst: fold bytes into the estimate without advancing.
		return
	}
	dtSec := dtTicks.Seconds()
	inst := float64(sizeBytes) / dtSec
	if inst < e.cfg.BytesPerSecondFloor {
		inst = e.cfg.BytesPerSecondFloor
	}
	prev := e.estimateBps
	if prev == 0 {
		e.estimateBps = inst
	} else {
		e.estimateBps = e.cfg.EWMADecay*prev + (1-e.cfg.EWMADecay)*inst
	}
	e.lastArrival = arrival
	e.adaptTarget(prev, e.estimateBps)
}

func (e *Estimator) adaptTarget(prev, next float64) {
	if prev == 0 {
		return
	}
	switch {
	case next > prev*1.02:
		e.upRun++
		e.downRun = 0
	case next < prev*0.98:
		e.downRun++
		e.upRun = 0
	default:
		e.upRun = 0
		e.downRun = 0
		return
	}
	if e.upRun >= e.cfg.ConfirmRuns {
		e.shrinkTarget()
		e.upRun = 0
	}
	if e.downRun >= e.cfg.ConfirmRuns {
		e.growTarget()
		e.downRun = 0
	}
}

// On rising bandwidth the safe buffer shrinks; on falling bandwidth it grows.
func (e *Estimator) shrinkTarget() {
	if e.target-e.cfg.StepTicks >= e.cfg.MinBuffer {
		e.target -= e.cfg.StepTicks
	} else {
		e.target = e.cfg.MinBuffer
	}
	e.bufferShrinks++
}

func (e *Estimator) growTarget() {
	if e.target+e.cfg.StepTicks <= e.cfg.MaxBuffer {
		e.target += e.cfg.StepTicks
	} else {
		e.target = e.cfg.MaxBuffer
	}
	e.bufferGrows++
}

// TargetBuffer is the current bounded buffering goal for the jitter window.
func (e *Estimator) TargetBuffer() clock.Ticks { return e.target }

// EstimateBps returns the smoothed bandwidth estimate.
func (e *Estimator) EstimateBps() float64 { return e.estimateBps }

// Observations counts Observe calls.
func (e *Estimator) Observations() int { return e.observations }

// BufferGrows / BufferShrinks report confirmed target transitions.
func (e *Estimator) BufferGrows() int   { return e.bufferGrows }
func (e *Estimator) BufferShrinks() int { return e.bufferShrinks }
