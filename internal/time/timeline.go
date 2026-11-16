// Package timeline normalizes presentation time: edit lists, non-zero
// and negative media-time origins, empty (gap) segments, PTS/DTS
// conversion, and 32-bit timestamp wrap detection.
package timeline

import (
	"mp4loc/internal/box"
)

// Segment is one edit-list entry mapped onto the presentation timeline.
type Segment struct {
	Start     int64 // presentation start, movie timescale units
	Duration  int64 // segment duration, movie timescale units
	MediaTime int64 // media-time origin of the segment; exactly -1 = empty
	Rate      float64
}

// Empty reports whether the segment is a gap.
func (s Segment) Empty() bool { return s.MediaTime == -1 }

// End returns the presentation time one past the segment.
func (s Segment) End() int64 { return s.Start + s.Duration }

// WrapEvent records one detected 32-bit timestamp wrap.
type WrapEvent struct {
	Sample    int   // sample index whose raw media time wrapped
	Raw       int64 // raw 32-bit-domain value
	Unwrapped int64 // normalized monotonic value
}

// Timeline is a track's normalized presentation clock.
type Timeline struct {
	Track          *box.Track
	MovieTimescale uint32
	Segments       []Segment
	Duration       int64 // total presentation duration, movie timescale
	MediaOrigin    int64 // media time at presentation zero (may be negative)
	Domain32       bool  // true when mdhd v0 => 32-bit timestamp domain
	WrapEvents     []WrapEvent
}

// Build constructs the timeline for a track. movieTimescale comes from
// mvhd; when the track has no edit list a single identity segment is
// synthesized from the media duration.
func Build(movieTimescale uint32, tr *box.Track) *Timeline {
	tl := &Timeline{Track: tr, MovieTimescale: movieTimescale, Domain32: tr.MdhdVersion == 0}
	if movieTimescale == 0 {
		movieTimescale = tr.Timescale
		tl.MovieTimescale = movieTimescale
	}
	var start int64
	if len(tr.Edits) == 0 {
		dur := rescale(tr.Duration, int64(tr.Timescale), int64(movieTimescale))
		tl.Segments = []Segment{{Start: 0, Duration: dur, MediaTime: 0, Rate: 1}}
		start = dur
	} else {
		for _, e := range tr.Edits {
			tl.Segments = append(tl.Segments, Segment{
				Start:     start,
				Duration:  e.SegmentDuration,
				MediaTime: e.MediaTime,
				Rate:      e.MediaRate,
			})
			start += e.SegmentDuration
		}
	}
	tl.Duration = start
	for _, s := range tl.Segments {
		if !s.Empty() {
			tl.MediaOrigin = s.MediaTime
			break
		}
	}
	tl.detectWraps()
	return tl
}

// detectWraps walks the monotonic media clock (MediaOrigin + DTS) and
// records every crossing of the 32-bit boundary. Raw values (the uint32
// projection) are kept alongside the unwrapped monotonic values.
func (tl *Timeline) detectWraps() {
	if !tl.Domain32 || tl.Track.Table == nil {
		return
	}
	const half = int64(1) << 31
	prev := int64(uint32(tl.MediaOrigin))
	for i := range tl.Track.Table.Samples {
		norm := tl.MediaOrigin + tl.Track.Table.Samples[i].DTS
		raw := int64(uint32(norm))
		if raw < prev && prev-raw > half {
			tl.WrapEvents = append(tl.WrapEvents, WrapEvent{Sample: i, Raw: raw, Unwrapped: norm})
		}
		prev = raw
	}
}

// Normalize maps a media DTS/PTS to (raw, normalized) media time. raw is
// the value as a 32-bit field would carry it (the uint32 projection of
// MediaOrigin + v) for 32-bit-domain tracks; normalized is the
// monotonic, presentation-zero-relative value — i.e. with the edit-list
// origin factored out. For 64-bit tracks raw == MediaOrigin + v.
func (tl *Timeline) Normalize(v int64) (raw, norm int64) {
	if tl.Domain32 {
		raw = int64(uint32(tl.MediaOrigin + v))
	} else {
		raw = tl.MediaOrigin + v
	}
	return raw, v
}

// LocateResult is the outcome of mapping a presentation target to media
// time. When OK is false, Reason explains why and Boundary names the
// nearest usable presentation time (movie timescale units).
type LocateResult struct {
	OK        bool
	MediaTime int64  // normalized media time, media timescale units
	Reason    string // "before_first_segment" | "empty_edit" | "beyond_duration"
	Boundary  int64  // nearest usable presentation time, movie timescale
}

// MapToMedia converts a presentation time (movie timescale units) to a
// normalized media time (media timescale units).
func (tl *Timeline) MapToMedia(pres int64) LocateResult {
	if len(tl.Segments) == 0 {
		return LocateResult{Reason: "beyond_duration", Boundary: 0}
	}
	first := tl.Segments[0]
	if pres < first.Start {
		return LocateResult{Reason: "before_first_segment", Boundary: first.Start}
	}
	for _, s := range tl.Segments {
		if pres >= s.Start && pres < s.End() {
			if s.Empty() {
				return LocateResult{Reason: "empty_edit", Boundary: s.End()}
			}
			offset := pres - s.Start
			if s.Rate != 0 && s.Rate != 1 {
				offset = int64(float64(offset) * s.Rate)
			}
			// offset is in movie-timescale units; MediaTime is media-timescale.
			offsetMedia := rescale(offset, int64(tl.MovieTimescale), int64(tl.Track.Timescale))
			return LocateResult{OK: true, MediaTime: s.MediaTime + offsetMedia}
		}
	}
	return LocateResult{Reason: "beyond_duration", Boundary: tl.Duration}
}

// SecondsToPres converts target seconds to movie-timescale units.
func (tl *Timeline) SecondsToPres(sec float64) int64 {
	return int64(sec * float64(tl.MovieTimescale))
}

// MediaToSeconds converts normalized media time to seconds.
func (tl *Timeline) MediaToSeconds(media int64) float64 {
	return float64(media) / float64(tl.Track.Timescale)
}

// rescale converts v from one timescale to another with rounding.
func rescale(v int64, from, to int64) int64 {
	if from == 0 || from == to {
		return v
	}
	return (v*to + from/2) / from
}
