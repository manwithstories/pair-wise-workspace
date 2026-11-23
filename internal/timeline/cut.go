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
	AfterCut int         // index of the cut before this transition
	Duration clock.Tick  // window length in ticks
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
// cut, snapping each cut boundary to the nearest sample start. Samples are not
// decoded or trimmed; only complete compressed samples survive.
func Select(in []*sample.Sample, plan Plan) Result {
	starts := make([]clock.Tick, len(in))
	for i, s := range in {
		starts[i] = s.PTS
	}
	res := Result{}
	kept := map[*sample.Sample]bool{}
	for _, cut := range plan.Cuts {
		startTick, devStart := snapAtOrBefore(starts, cut.Start)
		endTick, devEnd := snapAtOrAfter(starts, cut.End)
		res.Boundaries = append(res.Boundaries,
			Boundary{Requested: cut.Start, Snapped: startTick, Deviation: devStart},
			Boundary{Requested: cut.End, Snapped: endTick, Deviation: devEnd})
		for _, s := range in {
			if s.PTS >= startTick && s.PTS < endTick && !kept[s] {
				res.Samples = append(res.Samples, s)
				kept[s] = true
			}
		}
	}
	return res
}

// snapAtOrBefore returns the greatest sample start <= requested (or the first
// start), plus the signed deviation (snapped - requested).
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

// snapAtOrAfter returns the smallest sample start >= requested (or the last
// start), plus the signed deviation.
func snapAtOrAfter(starts []clock.Tick, requested clock.Tick) (clock.Tick, clock.Tick) {
	if len(starts) == 0 {
		return requested, 0
	}
	best := starts[len(starts)-1]
	for i := len(starts) - 1; i >= 0; i-- {
		if starts[i] >= requested {
			best = starts[i]
		}
	}
	return best, best - requested
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
