// Package metrics holds the small fixed-range histograms used for percentile
// reporting. Buckets are whole milliseconds so the results are stable across
// platforms and cheap to explain in the demo output.
package metrics

// Histogram counts observations in 1 ms buckets from 0 to maxMS inclusive.
// Values above maxMS are clamped into the last bucket.
type Histogram struct {
	maxMS  int64
	counts []int64
	count  int64
	sum    int64
}

// NewHistogram creates a histogram with an exclusive upper exponent.
func NewHistogram(maxMS int64) *Histogram {
	if maxMS <= 0 {
		maxMS = 1
	}
	return &Histogram{maxMS: maxMS, counts: make([]int64, maxMS+1)}
}

// Observe records one value in milliseconds.
func (h *Histogram) Observe(ms int64) {
	if ms < 0 {
		ms = 0
	}
	if ms > h.maxMS {
		ms = h.maxMS
	}
	h.counts[ms]++
	h.count++
	h.sum += ms
}

// Count returns the number of observations.
func (h *Histogram) Count() int64 { return h.count }

// Sum returns the arithmetic sum of observations.
func (h *Histogram) Sum() int64 { return h.sum }

// Percentile returns the nearest-rank percentile in milliseconds. An empty
// histogram returns 0.
func (h *Histogram) Percentile(p float64) int64 {
	if h.count == 0 {
		return 0
	}
	if p <= 0 {
		return 0
	}
	if p > 100 {
		p = 100
	}
	rank := int64(p/100*float64(h.count) + 0.999999)
	if rank < 1 {
		rank = 1
	}
	var seen int64
	for ms, c := range h.counts {
		seen += c
		if seen >= rank {
			return int64(ms)
		}
	}
	return h.maxMS
}

// Buckets returns a copy of the bucket counts for checkpointing.
func (h *Histogram) Buckets() []int64 {
	return append([]int64(nil), h.counts...)
}

// Restore reloads bucket counts previously exported by Buckets. It returns an
// error when the shape does not match.
func (h *Histogram) Restore(buckets []int64) error {
	if len(buckets) != len(h.counts) {
		return errShape
	}
	copy(h.counts, buckets)
	h.count, h.sum = 0, 0
	for ms, c := range h.counts {
		h.count += c
		h.sum += int64(ms) * c
	}
	return nil
}

type shapeError struct{}

func (shapeError) Error() string { return "metrics: histogram shape mismatch" }

var errShape = shapeError{}
