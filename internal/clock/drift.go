package clock

import "tscheck/internal/model"

// DriftEstimator estimates the ratio between an observed clock and a
// reference clock, clamped to a bounded ppm range. It never smooths or
// rewrites media intervals: raw VFR deltas stay untouched in the
// analysis layer.
type DriftEstimator struct {
	maxRatio float64 // max deviation from 1.0
	ref0     int64
	val0     int64
	init     bool
	drift    float64
}

// NewDriftEstimator creates an estimator bounded to ±maxPPM.
func NewDriftEstimator(maxPPM float64) *DriftEstimator {
	return &DriftEstimator{maxRatio: maxPPM / 1e6, drift: 1.0}
}

// Observe feeds a (reference, observed) tick pair and returns the
// clamped drift ratio (observed per reference tick, 1.0 = no drift).
func (d *DriftEstimator) Observe(ref, val model.Ticks) float64 {
	r, v := int64(ref), int64(val)
	if !d.init {
		d.ref0, d.val0, d.init = r, v, true
		return d.drift
	}
	dr, dv := r-d.ref0, v-d.val0
	if dr <= 0 || dv < 0 {
		return d.drift // non-monotonic reference: keep last estimate
	}
	ratio := float64(dv) / float64(dr)
	if ratio > 1+d.maxRatio {
		ratio = 1 + d.maxRatio
	} else if ratio < 1-d.maxRatio {
		ratio = 1 - d.maxRatio
	}
	d.drift = ratio
	return d.drift
}

// Drift returns the current clamped estimate.
func (d *DriftEstimator) Drift() float64 { return d.drift }
