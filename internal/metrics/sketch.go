package metrics

import (
	"math"
	"sort"
)

// latencySketch bounds percentile memory for unbounded streams: values are
// accumulated into a fixed number of log-spaced buckets whose boundaries can be
// reported to arbitrary precision. The p95 estimate is therefore approximate
// (within one bucket, under ~1% at the low end), which is appropriate for a
// latency SLO and keeps memory independent of duration.
type latencySketch struct {
	counts  []int64
	sum     float64
	n       int64
	maximum float64
	minSeen bool
	// Buckets are encoded as value = base * growth^index.
	base   float64
	growth float64
}

const sketchGrowth = 1.05

// add records one latency value in milliseconds.
func (s *latencySketch) add(v float64) {
	if s.counts == nil {
		s.base = 0.001 // 1 µs floor
		s.growth = sketchGrowth
		s.counts = make([]int64, 256)
	}
	if v < 0 {
		v = 0
	}
	if v > s.maximum {
		s.maximum = v
	}
	s.minSeen = true
	s.sum += v
	s.n++
	idx := s.index(v)
	if idx < 0 {
		idx = 0
	}
	for idx >= len(s.counts) {
		s.counts = append(s.counts, 0)
	}
	s.counts[idx]++
}

func (s *latencySketch) index(v float64) int {
	if v <= s.base {
		return 0
	}
	return int(math.Log(v/s.base) / math.Log(s.growth))
}

func (s *latencySketch) valueAt(idx int) float64 {
	return s.base * math.Pow(s.growth, float64(idx))
}

// percentile returns the smallest bucket upper bound at or above the rank.
func (s *latencySketch) percentile(p int) float64 {
	if s.n == 0 {
		return 0
	}
	target := int64(math.Ceil(float64(p) / 100 * float64(s.n)))
	if target < 1 {
		target = 1
	}
	var running int64
	for i, c := range s.counts {
		running += c
		if running >= target {
			// Use the upper edge of the bucket for a conservative estimate.
			return s.valueAt(i + 1)
		}
	}
	return s.maximum
}

func (s *latencySketch) max() float64 {
	if !s.minSeen {
		return 0
	}
	return s.maximum
}

// sortedSnapshot is retained for tests that need an exact comparison.
func (s *latencySketch) sortedSnapshot() []float64 {
	out := make([]float64, 0)
	for i, c := range s.counts {
		for k := int64(0); k < c; k++ {
			out = append(out, s.valueAt(i))
		}
	}
	sort.Float64s(out)
	return out
}
