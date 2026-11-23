// Package timeline applies cut points and transition windows in the unified
// 90 kHz timebase. A boundary that lands inside a compressed sample is always
// snapped outward so only whole samples are retained; the actual boundary
// deviation in ticks is reported for verification.
package timeline

import (
	"mediacore/internal/clock"
	"mediacore/internal/sample"
)

// Cut is one retained interval [Start, End) in unified ticks.
type Cut struct {
	Start clock.Tick
	End   clock.Tick
}

// Transition is an overlap window between two adjacent cuts, expressed as the
// duration extending backward from the next cut's start. Samples wholly inside
// the window are annotated but never split.
type Transition struct {
	AfterCut int        // index of the cut before this transition
	Duration clock.Tick // window length in ticks
}

// Plan is a validated edit plan.
type Plan struct {
	Cuts        []Cut
	Transitions []Transition
}

// Boundary reports the realized boundary after whole-sample snapping plus its
// signed deviation from the requested position.
type Boundary struct {
	Requested clock.Tick
	Snapped   clock.Tick
	Deviation clock.Tick
}

// Result holds selected samples and the realized boundary table.
type Result struct {
	Samples    []*sample.Sample
	Boundaries []Boundary
}

// Select keeps whole samples whose [PTS, PTS+Duration) intersects a retained
// cut, snapping each cut boundary to the nearest sample start *within each
// track independently*. Cross-track samples that share a PTS are distinct and
// are never merged. Samples are not decoded or trimmed; only complete
// compressed samples survive.
func Select(in []*sample.Sample, plan Plan) Result {
	byTrack := map[uint32][]*sample.Sample{}
	var trackOrder []uint32
	for _, s := range in {
		if _, ok := byTrack[s.TrackID]; !ok {
			trackOrder = append(trackOrder, s.TrackID)
		}
		byTrack[s.TrackID] = append(byTrack[s.TrackID], s)
	}
	res := Result{}
	kept := map[[2]int]bool{}
	for _, trackID := range trackOrder {
		trackSamples := byTrack[trackID]
		starts := make([]clock.Tick, len(trackSamples))
		for i, s := range trackSamples {
			starts[i] = s.PTS
		}
		for _, cut := range plan.Cuts {
			startTick, devStart := snapAtOrBefore(starts, cut.Start)
			endTick, devEnd := snapAtOrAfter(starts, cut.End)
			res.Boundaries = append(res.Boundaries,
				Boundary{Requested: cut.Start, Snapped: startTick, Deviation: devStart},
				Boundary{Requested: cut.End, Snapped: endTick, Deviation: devEnd})
			for _, s := range trackSamples {
				key := [2]int{int(s.TrackID), s.Seq}
				if s.PTS >= startTick && s.PTS < endTick && !kept[key] {
					res.Samples = append(res.Samples, s)
					kept[key] = true
				}
			}
		}
	}
	return res
}

// snapAtOrBefore returns the greatest sample start <= requested; when every
// start is later than requested it clamps to the first start.
func snapAtOrBefore(starts []clock.Tick, requested clock.Tick) (clock.Tick, clock.Tick) {
	if len(starts) == 0 {
		return requested, 0
	}
	best := starts[0]
	for _, s := range starts {
		if s <= requested {
			best = s
		}
	}
	return best, best - requested
}

// snapAtOrAfter returns the smallest sample start >= requested; when every
// start is earlier than requested it clamps to just past the last sample so a
// full-range cut retains the final sample instead of dropping it.
func snapAtOrAfter(starts []clock.Tick, requested clock.Tick) (clock.Tick, clock.Tick) {
	if len(starts) == 0 {
		return requested, 0
	}
	for _, s := range starts {
		if s >= requested {
			return s, s - requested
		}
	}
	last := starts[len(starts)-1]
	return last + 1, (last + 1) - requested
}

// MaxAbsDeviation returns the largest absolute boundary deviation in ticks,
// useful for asserting the snapping error stays within tolerance.
func (r Result) MaxAbsDeviation() clock.Tick {
	var max clock.Tick
	for _, b := range r.Boundaries {
		d := b.Deviation
		if d < 0 {
			d = -d
		}
		if d > max {
			max = d
		}
	}
	return max
}
