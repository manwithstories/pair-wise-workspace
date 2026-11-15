package ratecontrol

import "math"

// SegmentDemand is the first-pass information used by the second allocation.
type SegmentDemand struct {
	Frames     int
	Complexity float64
	HardCut    bool
}

// Allocate returns per-segment targets whose frame-weighted mean is targetKbps.
// Complexity and hard cuts receive more bits without changing the clip budget.
func Allocate(targetKbps float64, demands []SegmentDemand) []float64 {
	targets := make([]float64, len(demands))
	if targetKbps <= 0 || len(demands) == 0 {
		return targets
	}
	var weightedComplexity, totalFrames float64
	for _, d := range demands {
		if d.Frames <= 0 {
			continue
		}
		weightedComplexity += float64(d.Frames) * math.Max(1, d.Complexity)
		totalFrames += float64(d.Frames)
	}
	if weightedComplexity <= 0 {
		weightedComplexity = math.Max(1, totalFrames)
	}
	avgComplexity := weightedComplexity / math.Max(1, totalFrames)
	factors := make([]float64, len(demands))
	var weightedFactor, frames float64
	for i, d := range demands {
		if d.Frames <= 0 {
			continue
		}
		f := math.Pow(math.Max(0.05, d.Complexity)/avgComplexity, 1.15)
		if d.HardCut {
			f *= 1.08
		}
		f = math.Max(0.35, math.Min(1.70, f))
		factors[i] = f
		weightedFactor += float64(d.Frames) * f
		frames += float64(d.Frames)
	}
	if weightedFactor <= 0 {
		return targets
	}
	scale := targetKbps * frames / weightedFactor
	for i, d := range demands {
		if d.Frames > 0 {
			targets[i] = scale * factors[i]
		}
	}
	return targets
}
