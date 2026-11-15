// Package timeline converts between the presentation timeline (movie
// timescale, as defined by elst) and the media timeline (track
// timescale), and unwraps 32-bit timestamps into 64-bit ones.
package timeline

import "mp4probe/internal/moov"

// rateUnit is the 16.16 fixed-point unit used by elst rates.
const rateUnit = 1 << 16

// Segment is one resolved edit list segment with its cumulative
// presentation start precomputed.
type Segment struct {
	PresStart int64 // movie timescale
	PresDur   int64 // movie timescale
	MediaTime int64 // track timescale, -1 for empty edits
	Rate      int32 // 16.16 fixed point
}

// Empty reports whether the segment is an empty edit (dwell).
func (s Segment) Empty() bool { return s.MediaTime < 0 }

// Mapper translates presentation timestamps to media timestamps and
// back. With no edit list it falls back to a timescale conversion.
type Mapper struct {
	Segs       []Segment
	TotalPres  int64 // total presentation duration, movie timescale
	MovieScale uint32
	TrackScale uint32
	Conflict   bool // elst total disagreed with tkhd duration
}

// NewMapper builds a Mapper from a parsed edit list. When elst is
// empty the mapping is a pure timescale conversion.
func NewMapper(elst []moov.ElstEntry, movieScale, trackScale uint32, conflict bool) *Mapper {
	m := &Mapper{MovieScale: movieScale, TrackScale: trackScale, Conflict: conflict}
	var start int64
	for _, e := range elst {
		seg := Segment{PresStart: start, PresDur: e.Duration, MediaTime: e.MediaTime, Rate: e.Rate}
		if seg.Rate == 0 && !seg.Empty() {
			seg.Rate = rateUnit // treat broken zero rate as 1.0
		}
		m.Segs = append(m.Segs, seg)
		start += e.Duration
	}
	m.TotalPres = start
	return m
}

// ToMedia maps a presentation timestamp (movie timescale) to a media
// timestamp (track timescale). ok is false when the target falls into
// an empty edit or beyond the presented range.
func (m *Mapper) ToMedia(pres int64) (int64, bool) {
	if len(m.Segs) == 0 {
		return pres * int64(m.TrackScale) / int64(m.MovieScale), true
	}
	for _, s := range m.Segs {
		if pres < s.PresStart || pres >= s.PresStart+s.PresDur {
			continue
		}
		if s.Empty() {
			return 0, false
		}
		// Convert the in-segment presentation delta to track
		// timescale, then apply the segment rate.
		delta := (pres - s.PresStart) * int64(m.TrackScale) / int64(m.MovieScale)
		media := s.MediaTime + delta*int64(s.Rate)/rateUnit
		return media, true
	}
	return 0, false
}

// ToPresentation maps a media timestamp (track timescale) back to the
// presentation timeline (movie timescale). ok is false when the media
// time is not covered by any non-empty segment.
func (m *Mapper) ToPresentation(media int64) (int64, bool) {
	if len(m.Segs) == 0 {
		return media * int64(m.MovieScale) / int64(m.TrackScale), true
	}
	for _, s := range m.Segs {
		if s.Empty() || s.Rate == 0 {
			continue
		}
		// Media span covered by this segment, in track timescale.
		span := s.PresDur * int64(m.TrackScale) / int64(m.MovieScale) * int64(s.Rate) / rateUnit
		if media < s.MediaTime || media >= s.MediaTime+span {
			continue
		}
		// Undo the rate, then convert back to movie timescale.
		presDelta := (media - s.MediaTime) * rateUnit / int64(s.Rate)
		return s.PresStart + presDelta*int64(m.MovieScale)/int64(m.TrackScale), true
	}
	return 0, false
}

// SnapForward returns the media time at the start of the first
// non-empty segment at or after pres, for targets inside empty edits.
func (m *Mapper) SnapForward(pres int64) (int64, bool) {
	for _, s := range m.Segs {
		if s.Empty() || s.PresStart+s.PresDur <= pres {
			continue
		}
		start := s.PresStart
		if start < pres {
			start = pres
		}
		delta := (start - s.PresStart) * int64(m.TrackScale) / int64(m.MovieScale)
		return s.MediaTime + delta*int64(s.Rate)/rateUnit, true
	}
	return 0, false
}
