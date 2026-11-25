// Package timeline cuts multi-track access units into uniform segments on
// the shared rational time base. All tracks share one segment grid, so
// cross-track boundaries align exactly; holes inside a track become
// explicit gaps and are never filled with fabricated audio.
package timeline

import (
	"mediad/internal/clock"
	"mediad/internal/codec"
)

// Track is one source (studio, field, remote voice) on the timeline.
type Track struct {
	ID    string
	Units []codec.AccessUnit
}

// Gap is an explicit hole in one track's coverage.
type Gap struct {
	Track string     `json:"track"`
	Start clock.Time `json:"start"`
	End   clock.Time `json:"end"`
}

// Segment is a uniform slice of the multi-track timeline.
type Segment struct {
	Index int                            `json:"index"`
	Start clock.Time                     `json:"start"`
	End   clock.Time                     `json:"end"`
	Units map[string][]codec.AccessUnit `json:"units"` // by track ID
	Gaps  []Gap                          `json:"gaps,omitempty"`
}

// Segmentize cuts all tracks into segments of segDur on a common grid in
// the given base. Zero-duration units belong to the segment containing
// their start; out-of-range (negative) timestamps are clamped to zero.
func Segmentize(tracks []Track, segDur clock.Time, base clock.TimeBase) []Segment {
	segDur = segDur.Convert(base)
	if segDur.Ticks <= 0 {
		panic("timeline: non-positive segment duration")
	}
	var end int64
	for _, tr := range tracks {
		for _, u := range tr.Units {
			if e := u.End().Convert(base).Ticks; e > end {
				end = e
			}
		}
	}
	nseg := int((end + segDur.Ticks - 1) / segDur.Ticks)
	if nseg == 0 {
		return nil
	}
	segs := make([]Segment, nseg)
	for i := range segs {
		segs[i] = Segment{
			Index: i,
			Start: clock.At(int64(i)*segDur.Ticks, base),
			End:   clock.At(int64(i+1)*segDur.Ticks, base),
			Units: map[string][]codec.AccessUnit{},
		}
	}
	for _, tr := range tracks {
		var prevEnd int64 = -1
		for _, u := range tr.Units {
			start := u.Start.Convert(base).Ticks
			if start < 0 {
				start = 0 // out-of-range timestamp: clamp to origin
			}
			idx := int(start / segDur.Ticks)
			if idx >= nseg {
				idx = nseg - 1
			}
			segs[idx].Units[tr.ID] = append(segs[idx].Units[tr.ID], u)
			// Explicit gap between the previous unit's end and this start.
			unitEnd := u.End().Convert(base).Ticks
			if prevEnd >= 0 && start > prevEnd {
				segs[idx].Gaps = append(segs[idx].Gaps, Gap{
					Track: tr.ID,
					Start: clock.At(prevEnd, base),
					End:   clock.At(start, base),
				})
			}
			if unitEnd > prevEnd {
				prevEnd = unitEnd
			}
		}
	}
	return segs
}

// Coverage returns the fraction of [seg.Start, seg.End) covered by any unit
// of the given track (0..1), ignoring gaps.
func Coverage(seg Segment, trackID string) float64 {
	units := seg.Units[trackID]
	if len(units) == 0 {
		return 0
	}
	base := seg.Start.Base
	var covered int64
	cursor := seg.Start.Ticks
	for _, u := range units {
		s := u.Start.Convert(base).Ticks
		e := u.End().Convert(base).Ticks
		if s < cursor {
			s = cursor
		}
		if e > seg.End.Ticks {
			e = seg.End.Ticks
		}
		if e > s {
			covered += e - s
			cursor = e
		}
	}
	return float64(covered) / float64(seg.End.Ticks-seg.Start.Ticks)
}
