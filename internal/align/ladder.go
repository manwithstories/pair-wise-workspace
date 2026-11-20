// Package align aligns segment cut points across the bitrate ladder of a
// track. Missing keyframes or segments allow a bounded fallback to the
// nearest keyframe; anything beyond that becomes an explicit gap interval.
// Adjacent segments are never silently extended to cover a hole.
package align

import (
	"sort"

	"mediamap/internal/clock"
)

// Cut is one segment boundary on the unified 90 kHz timeline.
type Cut struct {
	Ticks    int64 `json:"ticks"`
	Keyframe bool  `json:"keyframe"`
	Derived  bool  `json:"derived,omitempty"`
}

// Rung is one representation's cut list.
type Rung struct {
	ID   string `json:"id"`
	Cuts []Cut  `json:"cuts"`
}

// Gap is an interval one rung could not cover.
type Gap struct {
	Rung   string `json:"rung"`
	Start  int64  `json:"start_ticks"`
	End    int64  `json:"end_ticks"`
	Reason string `json:"reason"`
}

// Result is the aligned ladder for one track.
type Result struct {
	Cuts    []int64 `json:"cuts"` // canonical cut times from the reference rung
	ErrorMs float64 `json:"error_ms"`
	Gaps    []Gap   `json:"gaps,omitempty"`
}

// Align uses rungs[0] as the reference. Every other rung must place a cut
// within tolerance of each reference cut; failing that it may fall back to
// a keyframe cut within maxFallback; failing that the interval is reported
// as a gap for that rung.
func Align(rungs []Rung, tolerance, maxFallback int64) Result {
	res := Result{}
	if len(rungs) == 0 {
		return res
	}
	for _, c := range rungs[0].Cuts {
		res.Cuts = append(res.Cuts, c.Ticks)
	}
	var maxDev int64
	for _, rung := range rungs[1:] {
		for i, ct := range res.Cuts {
			end := gapEnd(res.Cuts, i)
			idx := nearest(rung.Cuts, ct)
			if idx < 0 {
				res.Gaps = append(res.Gaps, Gap{Rung: rung.ID, Start: ct, End: end,
					Reason: "rung has no segments"})
				continue
			}
			if d := abs64(rung.Cuts[idx].Ticks - ct); d <= tolerance {
				if d > maxDev {
					maxDev = d
				}
				continue
			}
			kd, ok := nearestKeyframe(rung.Cuts, ct, maxFallback)
			if ok {
				if kd > maxDev {
					maxDev = kd
				}
				continue
			}
			res.Gaps = append(res.Gaps, Gap{Rung: rung.ID, Start: ct, End: end,
				Reason: "no cut within tolerance and no keyframe within fallback"})
		}
	}
	res.ErrorMs = clock.TicksToMillis(maxDev)
	return res
}

// gapEnd bounds a gap at the next canonical cut, or mirrors the previous
// interval, or closes a zero-width interval at the cut itself.
func gapEnd(cuts []int64, i int) int64 {
	switch {
	case i+1 < len(cuts):
		return cuts[i+1]
	case i > 0:
		return cuts[i] + (cuts[i] - cuts[i-1])
	default:
		return cuts[i]
	}
}

// nearest returns the index of the cut closest to t, or -1 for an empty
// rung. Cuts must be sorted ascending.
func nearest(cuts []Cut, t int64) int {
	if len(cuts) == 0 {
		return -1
	}
	i := sort.Search(len(cuts), func(j int) bool { return cuts[j].Ticks >= t })
	best, bestD := -1, int64(0)
	for _, j := range []int{i - 1, i} {
		if j < 0 || j >= len(cuts) {
			continue
		}
		if d := abs64(cuts[j].Ticks - t); best < 0 || d < bestD {
			best, bestD = j, d
		}
	}
	return best
}

// nearestKeyframe finds the keyframe cut closest to t within limit,
// returning its distance. Cuts must be sorted ascending.
func nearestKeyframe(cuts []Cut, t, limit int64) (int64, bool) {
	i := sort.Search(len(cuts), func(j int) bool { return cuts[j].Ticks >= t-limit })
	best, found := int64(0), false
	for ; i < len(cuts) && cuts[i].Ticks <= t+limit; i++ {
		if !cuts[i].Keyframe {
			continue
		}
		if d := abs64(cuts[i].Ticks - t); !found || d < best {
			best, found = d, true
		}
	}
	return best, found
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
