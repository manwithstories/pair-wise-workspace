package mpd

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// ErrOverflow marks any int64 overflow detected while accumulating or
// rescaling timeline values.
var ErrOverflow = errors.New("mpd: int64 overflow")

// Rescale converts v ticks from one timescale to another, rounding to the
// nearest tick (half up). It rejects negative values and overflows.
func Rescale(v, from, to int64) (int64, error) {
	if v < 0 || from <= 0 || to <= 0 {
		return 0, fmt.Errorf("mpd: invalid rescale args v=%d from=%d to=%d", v, from, to)
	}
	hi, lo := bits.Mul64(uint64(v), uint64(to))
	if hi != 0 {
		return 0, ErrOverflow
	}
	sum, carry := bits.Add64(lo, uint64(from)/2, 0)
	if carry != 0 {
		return 0, ErrOverflow
	}
	q, _ := bits.Div64(0, sum, uint64(from))
	if q > math.MaxInt64 {
		return 0, ErrOverflow
	}
	return int64(q), nil
}

// Accumulate returns len(deltas)+1 boundary ticks starting at start, adding
// one duration at a time with overflow checks.
func Accumulate(start int64, deltas []int64) ([]int64, error) {
	if start < 0 {
		return nil, fmt.Errorf("mpd: negative start %d", start)
	}
	out := make([]int64, len(deltas)+1)
	out[0] = start
	for i, d := range deltas {
		if d <= 0 {
			return nil, fmt.Errorf("mpd: non-positive duration %d at %d", d, i)
		}
		if d > math.MaxInt64-out[i] {
			return nil, ErrOverflow
		}
		out[i+1] = out[i] + d
	}
	return out, nil
}

// BuildTimeline run-length encodes n+1 boundary ticks into S elements. The
// first S carries the absolute T; later runs continue implicitly.
func BuildTimeline(boundaries []int64) (*SegmentTimeline, error) {
	if len(boundaries) < 2 {
		return nil, errors.New("mpd: need at least two boundaries")
	}
	tl := &SegmentTimeline{}
	for i := 0; i+1 < len(boundaries); {
		d := boundaries[i+1] - boundaries[i]
		if d <= 0 {
			return nil, fmt.Errorf("mpd: non-increasing boundary at %d", i)
		}
		run := int64(0)
		for j := i + 1; j+1 < len(boundaries) && boundaries[j+1]-boundaries[j] == d; j++ {
			run++
		}
		s := S{D: d}
		if len(tl.S) == 0 {
			t := boundaries[0]
			s.T = &t
		}
		if run > 0 {
			s.R = &run
		}
		tl.S = append(tl.S, s)
		i += int(run) + 1
	}
	return tl, nil
}

// SegmentPoint is one expanded segment: its Number (for $Number$), start
// tick T and duration D in the template timescale.
type SegmentPoint struct {
	Index  int
	Number int64
	T      int64
	D      int64
}

// ExpandTimeline unrolls S runs (honouring explicit T, repeat R and a
// non-1 startNumber) into per-segment points with overflow checks.
func ExpandTimeline(tl *SegmentTimeline, startNumber int64) ([]SegmentPoint, error) {
	if tl == nil || len(tl.S) == 0 {
		return nil, errors.New("mpd: empty SegmentTimeline")
	}
	if startNumber < 1 {
		return nil, fmt.Errorf("mpd: invalid startNumber %d", startNumber)
	}
	var pts []SegmentPoint
	var t int64
	num := startNumber
	for i, s := range tl.S {
		if s.D <= 0 {
			return nil, fmt.Errorf("mpd: S[%d] non-positive d=%d", i, s.D)
		}
		switch {
		case s.T != nil:
			t = *s.T
		case i == 0:
			return nil, errors.New("mpd: first S missing t")
		}
		r := int64(0)
		if s.R != nil {
			r = *s.R
			if r < 0 {
				return nil, fmt.Errorf("mpd: S[%d] negative r=%d", i, r)
			}
		}
		for k := int64(0); k <= r; k++ {
			pts = append(pts, SegmentPoint{Index: len(pts), Number: num, T: t, D: s.D})
			if s.D > math.MaxInt64-t {
				return nil, ErrOverflow
			}
			t += s.D
			if num == math.MaxInt64 {
				return nil, ErrOverflow
			}
			num++
		}
	}
	return pts, nil
}
