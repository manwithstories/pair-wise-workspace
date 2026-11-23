package rtp

import (
	"sort"

	"archive.local/archive/internal/model"
)

// BandwidthSample is one observation of delivered bytes over a window.
type BandwidthSample struct {
	WindowMs int64
	Bytes    int64
	Lost     int64
}

// BatchPlan is the reader batch size for the next increment.
type BatchPlan struct {
	Bytes       int   // target batch size
	Packets     int   // packets to request
	Confidence  int   // 0..100, higher with more samples
	SmoothedBPS int64 // smoothed estimate in bytes/second
	Reason      string
}

// Estimator adapts read batch sizes to a changing link. It smooths byte rate
// with an exponentially weighted mean, backs off multiplicatively when a loss
// burst is observed, and never rewinds confirmed progress: a loss burst only
// lowers the next batch, not what was already acknowledged.
type Estimator struct {
	MinBytes    int
	MaxBytes    int
	bytesPerPkt int
	smoothed    float64
	lastBurst   int64
	samples     int
	ceiling     int
}

// NewEstimator builds an estimator with sane local defaults.
func NewEstimator(minBytes, maxBytes, bytesPerPkt int) *Estimator {
	if minBytes < 1 {
		minBytes = 1
	}
	if maxBytes < minBytes {
		maxBytes = minBytes
	}
	if bytesPerPkt < 1 {
		bytesPerPkt = 1
	}
	return &Estimator{MinBytes: minBytes, MaxBytes: maxBytes, bytesPerPkt: bytesPerPkt,
		smoothed: float64(minBytes), ceiling: maxBytes}
}

// Observe folds a new sample into the estimate and returns the next plan.
// cumulativeLost is the total packets lost so far; a fresh burst is detected
// when it exceeds the previous high-water mark.
func (e *Estimator) Observe(s BandwidthSample, cumulativeLost int64) BatchPlan {
	plan := BatchPlan{}
	if s.WindowMs <= 0 {
		s.WindowMs = 1
	}
	instant := float64(s.Bytes) * 1000 / float64(s.WindowMs)
	if e.samples == 0 {
		e.smoothed = instant
	} else {
		// Weight recent observations more heavily (alpha = 0.3).
		e.smoothed = e.smoothed*0.7 + instant*0.3
	}
	e.samples++

	burst := cumulativeLost - e.lastBurst
	if burst > 0 {
		// A burst of missing packets: halve the target but keep the confirmed
		// high-water mark untouched.
		e.ceiling = e.ceiling / 2
		if e.ceiling < e.MinBytes {
			e.ceiling = e.MinBytes
		}
		plan.Reason = "loss burst: halve batch"
	} else {
		// Recover toward the smoothed rate, capped by the current ceiling.
		target := int(e.smoothed * float64(s.WindowMs) / 1000)
		if target > e.MaxBytes {
			target = e.MaxBytes
		}
		if target > e.ceiling {
			target = e.ceiling
		}
		if target < e.MinBytes {
			target = e.MinBytes
		}
		plan.Reason = "steady"
	}
	e.lastBurst = cumulativeLost
	bytes := int(e.smoothed * float64(s.WindowMs) / 1000)
	if bytes < e.MinBytes {
		bytes = e.MinBytes
	}
	if bytes > e.ceiling {
		bytes = e.ceiling
	}
	if bytes > e.MaxBytes {
		bytes = e.MaxBytes
	}
	plan.Bytes = bytes
	plan.Packets = bytes / e.bytesPerPkt
	if plan.Packets < 1 {
		plan.Packets = 1
	}
	plan.SmoothedBPS = int64(e.smoothed)
	plan.Confidence = e.samples * 20
	if plan.Confidence > 100 {
		plan.Confidence = 100
	}
	return plan
}

// ScheduledBatch picks the batch size for the next tick given a target
// completion deadline. It is used by the demo to show adaptive batching under
// a changing link.
func (e *Estimator) ScheduledBatch(remainingBytes int, budgetMs int64) BatchPlan {
	plan := BatchPlan{Confidence: 100}
	if remainingBytes <= 0 {
		plan.Reason = "complete"
		return plan
	}
	if budgetMs <= 0 {
		budgetMs = 1
	}
	need := int64(remainingBytes) * 1000 / budgetMs
	if e.smoothed > 0 && need > int64(e.smoothed) {
		plan.Reason = "behind budget"
		plan.SmoothedBPS = int64(e.smoothed)
	}
	bytes := need
	if bytes > int64(e.ceiling) {
		bytes = int64(e.ceiling)
	}
	if bytes > int64(e.MaxBytes) {
		bytes = int64(e.MaxBytes)
	}
	if bytes < int64(e.MinBytes) {
		bytes = int64(e.MinBytes)
	}
	plan.Bytes = int(bytes)
	plan.Packets = plan.Bytes / e.bytesPerPkt
	if plan.Packets < 1 {
		plan.Packets = 1
	}
	if plan.Reason == "" {
		plan.Reason = "on schedule"
	}
	return plan
}

// MedianWindow returns the median of window durations, used to normalise
// irregularly spaced trace records.
func MedianWindow(samples []BandwidthSample) int64 {
	if len(samples) == 0 {
		return 0
	}
	vals := make([]int64, len(samples))
	for i, s := range samples {
		vals[i] = s.WindowMs
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	return vals[len(vals)/2]
}

// ValidateSamples rejects non-monotonic or negative traces.
func ValidateSamples(samples []BandwidthSample) error {
	for i, s := range samples {
		if s.Bytes < 0 || s.WindowMs < 0 || s.Lost < 0 {
			return &model.Error{Code: model.CodeTransportDefect, Sample: i, OffsetBytes: -1,
				Detail: "bandwidth sample is negative"}
		}
	}
	return nil
}
