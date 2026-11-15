package forward

import (
	"math"
	"sort"
)

// JitterStats summarizes per-packet interarrival delay variation.
type JitterStats struct {
	Mean    float64
	Max     float64
	P99     float64
	Samples int
}

// JitterEstimator implements the RFC 3550 interarrival jitter estimate
// (J += (|D| - J) / 16) and keeps the per-packet |D| samples (in
// milliseconds) so quantiles can be computed offline.
type JitterEstimator struct {
	prevSend int64
	prevRecv int64
	started  bool
	j        float64
	samples  []float64
}

// NewJitterEstimator returns an estimator with room for cap samples.
func NewJitterEstimator(cap int) *JitterEstimator {
	return &JitterEstimator{samples: make([]float64, 0, cap)}
}

// Add records one packet with its send and receive timestamps (ns).
func (e *JitterEstimator) Add(sendNs, recvNs int64) {
	if !e.started {
		e.prevSend, e.prevRecv, e.started = sendNs, recvNs, true
		return
	}
	d := float64(recvNs-e.prevRecv) - float64(sendNs-e.prevSend)
	abs := math.Abs(d)
	e.j += (abs - e.j) / 16
	e.samples = append(e.samples, abs/1e6)
	e.prevSend, e.prevRecv = sendNs, recvNs
}

// Estimate returns the current RFC 3550 jitter estimate in ms.
func (e *JitterEstimator) Estimate() float64 { return e.j / 1e6 }

// Samples returns the recorded per-packet |D| values in ms.
func (e *JitterEstimator) Samples() []float64 { return e.samples }

// StatsOf computes mean, max and p99 over the given samples.
func StatsOf(samples []float64) JitterStats {
	st := JitterStats{Samples: len(samples)}
	if len(samples) == 0 {
		return st
	}
	sorted := make([]float64, len(samples))
	copy(sorted, samples)
	sort.Float64s(sorted)
	sum := 0.0
	for _, v := range sorted {
		sum += v
	}
	st.Mean = round1(sum / float64(len(sorted)))
	st.Max = round1(sorted[len(sorted)-1])
	idx := int(math.Ceil(0.99*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	st.P99 = round1(sorted[idx])
	return st
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }
