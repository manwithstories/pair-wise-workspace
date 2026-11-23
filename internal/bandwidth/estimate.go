// Package bandwidth maintains a per-block bitrate budget. The estimate is used
// only to size blocks and to declare an expected rate in the manifest; it never
// rewrites, truncates or pads coded samples.
package bandwidth

import (
	"fmt"
	"math"
)

// Estimator tracks a target bitrate from observed block throughput with an
// exponential moving average.
type Estimator struct {
	// TargetKbps is the operator setpoint.
	TargetKbps float64
	// Alpha weights the newest block; 0 < Alpha <= 1.
	Alpha float64
	// Current is the smoothed estimate in kbps.
	Current float64

	blocks        int
	totalBits     float64
	totalSec      float64
	lastBlockKbps float64
}

// New returns an estimator seeded at the target.
func New(targetKbps float64, alpha float64) (*Estimator, error) {
	if targetKbps <= 0 {
		return nil, fmt.Errorf("bandwidth: target must be positive")
	}
	if alpha <= 0 || alpha > 1 {
		return nil, fmt.Errorf("bandwidth: alpha must be in (0,1]")
	}
	return &Estimator{TargetKbps: targetKbps, Alpha: alpha, Current: targetKbps}, nil
}

// ObserveBlock updates the estimate from one block's byte count and duration.
func (e *Estimator) ObserveBlock(bytes int, seconds float64) {
	if bytes < 0 || seconds <= 0 {
		return
	}
	kbps := float64(bytes) * 8 / 1000 / seconds
	if e.blocks == 0 {
		e.Current = kbps
	} else {
		e.Current = e.Alpha*kbps + (1-e.Alpha)*e.Current
	}
	e.blocks++
	e.totalBits += float64(bytes) * 8
	e.totalSec += seconds
	e.lastBlockKbps = kbps
}

// AverageKbps is the observed rate over every block seen so far.
func (e *Estimator) AverageKbps() float64 {
	if e.totalSec == 0 {
		return 0
	}
	return e.totalBits / 1000 / e.totalSec
}

// LastBlockKbps is the most recent block's instantaneous rate.
func (e *Estimator) LastBlockKbps() float64 { return e.lastBlockKbps }

// Blocks returns the number of observed blocks.
func (e *Estimator) Blocks() int { return e.blocks }

// DeviationPct is the signed deviation of the observed average from the target.
func (e *Estimator) DeviationPct() float64 {
	avg := e.AverageKbps()
	if avg == 0 || e.TargetKbps == 0 {
		return 0
	}
	return (avg - e.TargetKbps) / e.TargetKbps * 100
}

// PlanBlockSamples picks how many samples fit in the next block given the
// current estimate. It never returns less than one sample.
func (e *Estimator) PlanBlockSamples(maxSamples int, avgSampleBytes int) int {
	if maxSamples <= 0 {
		return 0
	}
	if avgSampleBytes <= 0 {
		return maxSamples
	}
	// Bytes allowed in one second of media at the current estimate.
	budgetBytes := e.Current * 1000 / 8
	fit := int(budgetBytes / float64(avgSampleBytes))
	if fit < 1 {
		fit = 1
	}
	if fit > maxSamples {
		fit = maxSamples
	}
	return fit
}

// Finite reports whether the estimate is a usable number.
func (e *Estimator) Finite() bool {
	return !math.IsNaN(e.Current) && !math.IsInf(e.Current, 0)
}
