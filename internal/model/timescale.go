package model

import (
	"fmt"
	"math"
	"math/big"
)

// Segment is a fully expanded timeline entry in timescale ticks.
type Segment struct {
	Number uint64
	T      uint64
	D      uint64
}

// RatFromMS converts whole milliseconds to an exact rational of seconds.
func RatFromMS(ms int64) *big.Rat { return new(big.Rat).SetFrac64(ms, 1000) }

// TicksToMS projects ticks onto the unified 1/1000 timebase, exactly.
func TicksToMS(ticks *big.Rat, timescale uint64) *big.Rat {
	return new(big.Rat).Mul(ticks, new(big.Rat).SetFrac64(1000, int64(timescale)))
}

// PresentationTicks maps media time to presentation time: t - pto.
// The result may be negative; callers decide whether that is an error.
func PresentationTicks(t, pto *big.Rat) *big.Rat { return new(big.Rat).Sub(t, pto) }

// AbsPresentationMS combines Period@start with PTO: start*1000 + (t-pto)*1000/timescale.
func AbsPresentationMS(periodStart *big.Rat, t, pto uint64, timescale uint64) *big.Rat {
	media := new(big.Rat).SetUint64(t)
	off := new(big.Rat).SetUint64(pto)
	rel := TicksToMS(PresentationTicks(media, off), timescale)
	return rel.Add(rel, new(big.Rat).Mul(periodStart, big.NewRat(1000, 1)))
}

// MSToTicksExact converts milliseconds to ticks, reporting any remainder.
func MSToTicksExact(ms int64, timescale uint64) (ticks int64, exact bool) {
	num := ms * int64(timescale)
	return num / 1000, num%1000 == 0
}

// ExpandTimeline unrolls S runs into concrete segments. The first S may
// carry @t for pre-roll; later @t values are checked for regression and
// gaps. All arithmetic is integer; no floats are involved.
func ExpandTimeline(tl *SegmentTimeline, startNumber uint64) ([]Segment, []Problem) {
	var segs []Segment
	var probs []Problem
	var cur uint64
	n := startNumber
	for i, s := range tl.S {
		if s.D == 0 {
			probs = append(probs, Problem{Code: CodeZeroDuration, Msg: fmt.Sprintf("S[%d] @d=0", i)})
			continue
		}
		if s.R < 0 {
			probs = append(probs, Problem{Code: CodeOutOfRange, Msg: fmt.Sprintf("S[%d] negative @r unsupported", i)})
			continue
		}
		t := cur
		if s.T != nil {
			t = *s.T
			if i > 0 {
				if t < cur {
					probs = append(probs, Problem{Code: CodeTimeRegression, Msg: fmt.Sprintf("S[%d] @t=%d goes back before %d", i, t, cur)})
				} else if t > cur {
					probs = append(probs, Problem{Code: CodeMissingSegment, Msg: fmt.Sprintf("S[%d] @t=%d skips [%d,%d)", i, t, cur, t)})
				}
			}
		}
		for j := int64(0); j <= s.R; j++ {
			segs = append(segs, Segment{Number: n, T: t, D: s.D})
			n++
			t += s.D
		}
		cur = t
	}
	return segs, probs
}

// MergeRuns folds equal durations into @r runs; runs longer than uint32
// are split into consecutive S elements.
func MergeRuns(durs []uint64, t0 uint64) *SegmentTimeline {
	tl := &SegmentTimeline{}
	var cur *S
	for i, d := range durs {
		if cur != nil && cur.D == d && cur.R < math.MaxUint32 {
			cur.R++
			continue
		}
		s := &S{D: d}
		if i == 0 && t0 != 0 {
			t := t0
			s.T = &t
		}
		tl.S = append(tl.S, s)
		cur = s
	}
	return tl
}

// TimelineProblems re-expands a timeline and returns any findings.
func TimelineProblems(tl *SegmentTimeline, startNumber uint64) []Problem {
	_, probs := ExpandTimeline(tl, startNumber)
	return probs
}
