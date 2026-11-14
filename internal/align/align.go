// Package align normalizes representation timelines onto a common
// presentation clock and measures cross-representation boundary deviation.
package align

import (
	"fmt"
	"sort"

	"dashpack/internal/mp4"
)

// RepMeta describes one representation directory under the fixture root.
type RepMeta struct {
	ID        string `json:"id"`
	Dir       string `json:"dir"`
	Bandwidth uint64 `json:"bandwidth"`
}

// RepResult is the alignment outcome for one representation.
type RepResult struct {
	ID                     string  `json:"id"`
	Bandwidth              uint64  `json:"bandwidth"`
	Timescale              uint32  `json:"timescale"`
	PresentationTimeOffset uint64  `json:"presentationTimeOffset"`
	MaxDeviationMs         float64 `json:"max_deviation_ms"`
	MeanDeviationMs        float64 `json:"mean_deviation_ms"`
	Samples                int     `json:"samples"`
}

// Result is the full cross-representation alignment outcome.
type Result struct {
	ReferenceID     string      `json:"reference"`
	ThresholdMs     float64     `json:"threshold_ms"`
	MaxDeviationMs  float64     `json:"max_deviation_ms"`
	MeanDeviationMs float64     `json:"mean_deviation_ms"`
	Reps            []RepResult `json:"reps"`
	OutOfTolerance  []string    `json:"out_of_tolerance,omitempty"`
}

// Align picks the highest-bandwidth representation as the reference clock,
// derives each representation's presentationTimeOffset from its first
// segment, and measures per-boundary deviation in milliseconds.
func Align(metas []RepMeta, tls map[string]*mp4.Timeline, thresholdMs float64) (*Result, error) {
	if len(metas) == 0 {
		return nil, fmt.Errorf("align: no representations")
	}
	sorted := make([]RepMeta, len(metas))
	copy(sorted, metas)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Bandwidth != sorted[j].Bandwidth {
			return sorted[i].Bandwidth > sorted[j].Bandwidth
		}
		return sorted[i].ID < sorted[j].ID
	})
	ref := sorted[0]
	refTL := tls[ref.ID]
	if refTL == nil || len(refTL.Segments) == 0 {
		return nil, fmt.Errorf("align: reference %q has no segments", ref.ID)
	}
	res := &Result{ReferenceID: ref.ID, ThresholdMs: thresholdMs}
	refPTO := refTL.Segments[0].Start
	refBounds := make([]float64, len(refTL.Segments))
	for i, s := range refTL.Segments {
		refBounds[i] = toMs(s.Start-refPTO, refTL.Timescale)
	}
	nominalMs := toMs(refTL.NominalDuration(), refTL.Timescale)
	halfWindow := nominalMs / 2
	var devSum float64
	var devCount int
	for _, meta := range sorted {
		tl := tls[meta.ID]
		if tl == nil || len(tl.Segments) == 0 {
			return nil, fmt.Errorf("align: representation %q has no segments", meta.ID)
		}
		rep := RepResult{
			ID:                     meta.ID,
			Bandwidth:              meta.Bandwidth,
			Timescale:              tl.Timescale,
			PresentationTimeOffset: tl.Segments[0].Start,
		}
		if meta.ID != ref.ID {
			pto := tl.Segments[0].Start
			j := 0
			var sum float64
			var n int
			for _, s := range tl.Segments {
				ms := toMs(s.Start-pto, tl.Timescale)
				for j+1 < len(refBounds) && refBounds[j+1] <= ms {
					j++
				}
				best := -1.0
				for _, k := range []int{j, j + 1} {
					if k < 0 || k >= len(refBounds) {
						continue
					}
					d := ms - refBounds[k]
					if d < 0 {
						d = -d
					}
					if best < 0 || d < best {
						best = d
					}
				}
				if best < 0 || best > halfWindow {
					continue // no matching reference boundary (e.g. across a gap)
				}
				dev := ms - nearestRef(ms, refBounds)
				if dev < 0 {
					if -dev > rep.MaxDeviationMs {
						rep.MaxDeviationMs = -dev
					}
				} else if dev > rep.MaxDeviationMs {
					rep.MaxDeviationMs = dev
				}
				if dev < 0 {
					dev = -dev
				}
				sum += dev
				n++
			}
			if n > 0 {
				rep.Samples = n
				rep.MeanDeviationMs = round3(sum / float64(n))
				rep.MaxDeviationMs = round3(rep.MaxDeviationMs)
				devSum += sum
				devCount += n
			}
			if rep.MaxDeviationMs > thresholdMs {
				res.OutOfTolerance = append(res.OutOfTolerance, rep.ID)
			}
		}
		res.Reps = append(res.Reps, rep)
		if rep.MaxDeviationMs > res.MaxDeviationMs {
			res.MaxDeviationMs = rep.MaxDeviationMs
		}
	}
	if devCount > 0 {
		res.MeanDeviationMs = round3(devSum / float64(devCount))
	}
	res.MaxDeviationMs = round3(res.MaxDeviationMs)
	return res, nil
}

// toMs converts timescale units to fractional milliseconds.
func toMs(v uint64, timescale uint32) float64 {
	if timescale == 0 {
		return 0
	}
	return float64(v) * 1000 / float64(timescale)
}

func nearestRef(ms float64, bounds []float64) float64 {
	best := bounds[0]
	bestD := ms - best
	if bestD < 0 {
		bestD = -bestD
	}
	for _, b := range bounds[1:] {
		d := ms - b
		if d < 0 {
			d = -d
		}
		if d < bestD {
			best, bestD = b, d
		}
	}
	return best
}

func round3(f float64) float64 {
	if f < 0 {
		return -round3(-f)
	}
	return float64(int(f*1000+0.5)) / 1000
}
