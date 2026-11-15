// Package timeline projects every representation's segment boundaries onto
// the unified 1/1000 timebase and measures cross-rung deviation.
package timeline

import (
	"fmt"
	"math/big"
	"sort"

	"dashkit/internal/model"
)

// Deviation locates one misaligned switch point.
type Deviation struct {
	TimeMS      *big.Rat
	RepID       string
	DeviationMS *big.Rat
}

// Alignment summarizes cross-rung alignment for one manifest.
type Alignment struct {
	MaxDeviationMS *big.Rat
	SwitchPoints   int
	Mismatches     int
	PTODriftTicks  *big.Rat
	Worst          []Deviation // up to 3, worst first
}

// Align compares switch points of every rung against the reference rung
// (lowest ID) per period. All math is exact big.Rat on the 1/1000
// timebase; no floating point accumulation occurs anywhere.
func Align(m *model.MPD) (*Alignment, []model.Problem) {
	al := &Alignment{MaxDeviationMS: new(big.Rat), PTODriftTicks: new(big.Rat)}
	var probs []model.Problem
	for pi, period := range m.Periods {
		reps := sortedReps(period)
		if len(reps) == 0 {
			continue
		}
		bounds := make(map[string][]*big.Rat, len(reps))
		drifts := make(map[string]*big.Rat, len(reps))
		for _, rep := range reps {
			bs, ps, drift := boundaries(period, rep, pi)
			probs = append(probs, ps...)
			bounds[rep.ID] = bs
			drifts[rep.ID] = drift
		}
		al.PTODriftTicks = maxRat(al.PTODriftTicks, spread(drifts))
		ref := bounds[reps[0].ID]
		al.SwitchPoints += len(ref)
		for _, rb := range ref {
			var worst *big.Rat
			worstRep := ""
			for _, rep := range reps[1:] {
				d := nearest(rb, bounds[rep.ID])
				if d.Sign() > 0 && (worst == nil || d.Cmp(worst) > 0) {
					worst, worstRep = d, rep.ID
				}
			}
			if worst == nil {
				continue
			}
			al.Mismatches++
			if worst.Cmp(al.MaxDeviationMS) > 0 {
				al.MaxDeviationMS = worst
			}
			al.Worst = append(al.Worst, Deviation{TimeMS: rb, RepID: worstRep, DeviationMS: worst})
		}
	}
	sort.SliceStable(al.Worst, func(i, j int) bool {
		return al.Worst[i].DeviationMS.Cmp(al.Worst[j].DeviationMS) > 0
	})
	if len(al.Worst) > 3 {
		al.Worst = al.Worst[:3]
	}
	return al, probs
}

func sortedReps(p *model.Period) []*model.Representation {
	var reps []*model.Representation
	for _, as := range p.Sets {
		reps = append(reps, as.Reps...)
	}
	sort.SliceStable(reps, func(i, j int) bool { return reps[i].ID < reps[j].ID })
	return reps
}

// boundaries returns absolute presentation-time (ms) of every segment
// start, plus the rep's (t0 - pto) drift in ticks.
func boundaries(period *model.Period, rep *model.Representation, pi int) ([]*big.Rat, []model.Problem, *big.Rat) {
	tpl := rep.Template
	segs, probs := model.ExpandTimeline(tpl.Timeline, tpl.StartNumber)
	for i := range probs {
		probs[i].Rep = rep.ID
	}
	var out []*big.Rat
	var drift *big.Rat
	for _, s := range segs {
		t := new(big.Rat).SetUint64(s.T)
		pto := tpl.PresentationTimeOffset
		if pto == nil {
			pto = new(big.Rat)
		}
		rel := model.PresentationTicks(t, pto)
		if rel.Sign() < 0 {
			probs = append(probs, model.Problem{Code: model.CodeNegativeTime, Rep: rep.ID,
				Msg: fmt.Sprintf("period %d segment #%d presentation time %s ticks", pi, s.Number, rel.RatString())})
		}
		if drift == nil {
			drift = rel
		}
		abs := model.TicksToMS(rel, tpl.Timescale)
		if period.Start != nil {
			abs.Add(abs, new(big.Rat).Mul(period.Start, big.NewRat(1000, 1)))
		}
		out = append(out, abs)
	}
	return out, probs, drift
}

// nearest returns the distance from t to the closest boundary in bs.
func nearest(t *big.Rat, bs []*big.Rat) *big.Rat {
	var best *big.Rat
	for _, b := range bs {
		d := new(big.Rat).Sub(b, t)
		d.Abs(d)
		if best == nil || d.Cmp(best) < 0 {
			best = d
		}
	}
	if best == nil {
		return new(big.Rat)
	}
	return best
}

func maxRat(a, b *big.Rat) *big.Rat {
	if b != nil && b.Cmp(a) > 0 {
		return b
	}
	return a
}

// spread returns max-min of the given drift values.
func spread(m map[string]*big.Rat) *big.Rat {
	var lo, hi *big.Rat
	for _, v := range m {
		if v == nil {
			continue
		}
		if lo == nil || v.Cmp(lo) < 0 {
			lo = v
		}
		if hi == nil || v.Cmp(hi) > 0 {
			hi = v
		}
	}
	if lo == nil {
		return new(big.Rat)
	}
	return new(big.Rat).Sub(hi, lo)
}
