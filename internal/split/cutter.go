package split

import (
	"fmt"
	"math"
	"sort"
)

// FragModel is one fragment's sample layout on a track timeline.
type FragModel struct {
	StartUnits uint64   // tfdt as stated by the fragment
	Samples    []uint32 // per-sample durations in timescale units
}

// Cutter rounds cut positions to sample boundaries deterministically.
type Cutter struct {
	Timescale  uint32
	boundaries []uint64 // sorted sample boundary positions in units
}

// NewCutter builds the boundary table from the fragment layout.
func NewCutter(timescale uint32, frags []FragModel) (*Cutter, error) {
	if timescale == 0 {
		return nil, fmt.Errorf("split: timescale must be positive")
	}
	var b []uint64
	for _, f := range frags {
		pos := f.StartUnits
		b = append(b, pos)
		for _, d := range f.Samples {
			pos += uint64(d)
			if pos < f.StartUnits { // uint64 wraparound
				return nil, fmt.Errorf("split: sample positions overflow at tfdt %d", f.StartUnits)
			}
			b = append(b, pos)
		}
	}
	sort.Slice(b, func(i, j int) bool { return b[i] < b[j] })
	// Deduplicate boundaries shared between adjacent fragments.
	out := b[:0]
	for i, v := range b {
		if i == 0 || v != b[i-1] {
			out = append(out, v)
		}
	}
	return &Cutter{Timescale: timescale, boundaries: out}, nil
}

// Round maps a unified-ms position to the nearest sample boundary.
// Ties resolve to the boundary with the even index (round half to
// even), so the result is deterministic and platform-independent.
// It returns the actual boundary in units and milliseconds.
func (c *Cutter) Round(ms float64) (units uint64, actualMs float64, err error) {
	if len(c.boundaries) == 0 {
		return 0, 0, fmt.Errorf("split: no sample boundaries")
	}
	if ms < 0 {
		ms = 0
	}
	target := ms * float64(c.Timescale) / 1000.0
	i := sort.Search(len(c.boundaries), func(i int) bool {
		return float64(c.boundaries[i]) >= target
	})
	best := 0
	switch {
	case i == 0:
		best = 0
	case i == len(c.boundaries):
		best = len(c.boundaries) - 1
	default:
		lo, hi := c.boundaries[i-1], c.boundaries[i]
		dLo := target - float64(lo)
		dHi := float64(hi) - target
		switch {
		case dLo < dHi:
			best = i - 1
		case dHi < dLo:
			best = i
		default: // exact tie: round half to even
			if (i-1)%2 == 0 {
				best = i - 1
			} else {
				best = i
			}
		}
	}
	units = c.boundaries[best]
	return units, float64(units) * 1000.0 / float64(c.Timescale), nil
}

// Segment is one cut piece with requested and actual offsets.
type Segment struct {
	Track      string
	Index      int
	ReqStartMs float64
	ReqEndMs   float64
	ActStartMs float64
	ActEndMs   float64
	StartUnits uint64
	EndUnits   uint64
}

// Cut slices [0, totalMs) into intervalMs-long segments, rounding each
// boundary to a sample edge. The actual offsets are reported, never
// silently substituted: Req* keeps the request, Act* the result.
func Cut(track string, c *Cutter, totalMs, intervalMs float64) ([]Segment, error) {
	if intervalMs <= 0 || totalMs <= 0 {
		return nil, fmt.Errorf("split: invalid range total=%v interval=%v", totalMs, intervalMs)
	}
	n := int(math.Ceil(totalMs / intervalMs))
	segs := make([]Segment, 0, n)
	for k := 0; k < n; k++ {
		reqStart := float64(k) * intervalMs
		reqEnd := math.Min(reqStart+intervalMs, totalMs)
		su, actStart, err := c.Round(reqStart)
		if err != nil {
			return nil, err
		}
		eu, actEnd, err := c.Round(reqEnd)
		if err != nil {
			return nil, err
		}
		if eu <= su && reqEnd > reqStart {
			// Degenerate after rounding: skip empty segments but keep
			// the run deterministic.
			continue
		}
		segs = append(segs, Segment{
			Track: track, Index: k,
			ReqStartMs: reqStart, ReqEndMs: reqEnd,
			ActStartMs: actStart, ActEndMs: actEnd,
			StartUnits: su, EndUnits: eu,
		})
	}
	return segs, nil
}
