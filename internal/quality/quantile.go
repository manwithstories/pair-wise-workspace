package quality

import (
	"math"
	"sort"

	"mediac/internal/inspect"
	"mediac/internal/manifest"
)

// quantilePercentiles are reported by default (median, p90, p95).
var quantilePercentiles = []int{50, 90, 95}

// gopQuantiles converts GOP tick intervals to milliseconds at each fragment's
// timescale and computes the configured percentiles. Clock resets never reach
// here because inspect.KeyframeGOPs already drops backwards spans.
func gopQuantiles(reports []*inspect.FragmentReport) map[int]float64 {
	gops := inspect.KeyframeGOPs(reports)
	if len(gops) == 0 {
		return map[int]float64{}
	}
	valuesMs := make([]float64, 0, len(gops))
	for _, g := range gops {
		// Keyframe intervals sit within one fragment in the fixture; use that
		// fragment's timescale. Fall back to 1000 when the lookup is empty.
		scale := uint32(1000)
		for _, r := range reports {
			if r.Timescale > 0 {
				scale = r.Timescale
				break
			}
		}
		ms := float64(g.Ticks) * 1000 / float64(scale)
		valuesMs = append(valuesMs, ms)
	}
	sort.Float64s(valuesMs)
	out := map[int]float64{}
	for _, p := range quantilePercentiles {
		out[p] = round2(percentile(valuesMs, p))
	}
	return out
}

// percentile returns the nearest-rank percentile of a sorted slice.
func percentile(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(float64(p)/100*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// alignmentError measures the largest start-time difference for segments at
// the same sequence index across representations. DASH/HLS renditions of one
// program must share segment boundaries; a large drift indicates a manifest
// or muxing defect rather than a transport loss.
func alignmentError(m *manifest.Manifest, repIndex int) int64 {
	ref := m.Representations[repIndex]
	var maxDrift int64
	for ri, other := range m.Representations {
		if ri == repIndex {
			continue
		}
		n := len(ref.Segments)
		if len(other.Segments) < n {
			n = len(other.Segments)
		}
		for i := 0; i < n; i++ {
			if ref.Segments[i].Index != other.Segments[i].Index {
				continue
			}
			drift := abs64(ref.Segments[i].StartMs - other.Segments[i].StartMs)
			if drift > maxDrift {
				maxDrift = drift
			}
		}
	}
	return maxDrift
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
