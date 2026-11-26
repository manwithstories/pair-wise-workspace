// Package analyze turns parsed sample tables into track structure and
// keyframe interval summaries. It never decodes media.
package analyze

import (
	"mediascan/internal/media"
	"mediascan/internal/parser"
)

// TimePoint is a media timestamp in both native and unified time bases.
type TimePoint struct {
	Ticks           int64          `json:"ticks"`
	Timescale       uint32         `json:"timescale"`
	UnifiedTicks    int64          `json:"unified_ticks"`
	UnifiedTimeBase uint32         `json:"unified_time_base"`
	UnifiedValid    bool           `json:"unified_valid"`
	Seconds         media.Rational `json:"seconds"`
}

// EditSummary is the report view of an edit-list entry.
type EditSummary struct {
	SegmentDuration uint64 `json:"segment_duration"`
	MediaTime       int64  `json:"media_time"`
	MediaRate       uint32 `json:"media_rate"`
}

// Stream is the structural summary for one track.
type Stream struct {
	FileIndex     int           `json:"file_index"`
	File          string        `json:"file"`
	FileSHA256    string        `json:"file_sha256"`
	TrackID       uint32        `json:"track_id"`
	Handler       string        `json:"handler"`
	Timescale     uint32        `json:"timescale"`
	DurationTicks uint64        `json:"duration_ticks"`
	SampleCount   int           `json:"sample_count"`
	FragmentCount int           `json:"fragment_count"`
	FirstTime     *TimePoint    `json:"first_time,omitempty"`
	LastTime      *TimePoint    `json:"last_time,omitempty"`
	EndTime       *TimePoint    `json:"end_time,omitempty"`
	Edits         []EditSummary `json:"edits,omitempty"`
	EditStartTime *TimePoint    `json:"edit_start_time,omitempty"`
	ZeroTimescale bool          `json:"zero_timescale"`
}

// Point builds a TimePoint with exact native rational and scaled unified ticks.
func Point(t media.Time) TimePoint {
	u, ok := t.Unified()
	return TimePoint{
		Ticks:           t.Ticks,
		Timescale:       t.Timescale,
		UnifiedTicks:    u,
		UnifiedTimeBase: media.UnifiedTimescale,
		UnifiedValid:    ok,
		Seconds:         t.Rational(),
	}
}

// AnalyzeStreams summarizes every track in deterministic track-ID order.
func AnalyzeStreams(pf *parser.File, fileIndex int) []Stream {
	if pf == nil {
		return nil
	}
	out := make([]Stream, 0, len(pf.Tracks))
	for _, t := range pf.Tracks {
		out = append(out, streamForTrack(pf, t, fileIndex))
	}
	return out
}

func streamForTrack(pf *parser.File, t *parser.Track, fileIndex int) Stream {
	s := Stream{
		FileIndex:     fileIndex,
		File:          pf.Source.Name,
		FileSHA256:    pf.Source.SHA256,
		TrackID:       t.TrackID,
		Handler:       t.Handler,
		Timescale:     t.Timescale,
		DurationTicks: t.Duration,
		SampleCount:   trackSampleCount(t),
		FragmentCount: len(t.Fragments),
		ZeroTimescale: t.Timescale == 0,
	}
	for _, e := range t.Edits {
		s.Edits = append(s.Edits, EditSummary{SegmentDuration: e.SegmentDuration, MediaTime: e.MediaTime, MediaRate: e.MediaRate})
	}
	if len(t.Edits) > 0 {
		mt := media.NewTime(t.Edits[0].MediaTime, t.Timescale)
		p := Point(mt)
		s.EditStartTime = &p
	}

	if first, last, end, ok := trackTimeRange(t); ok {
		fp := Point(media.NewTime(first, t.Timescale))
		lp := Point(media.NewTime(last, t.Timescale))
		ep := Point(media.NewTime(end, t.Timescale))
		s.FirstTime = &fp
		s.LastTime = &lp
		s.EndTime = &ep
	}
	return s
}

func trackSampleCount(t *parser.Track) int {
	if len(t.Samples) > 0 {
		return len(t.Samples)
	}
	n := 0
	for _, f := range t.Fragments {
		n += len(f.Samples)
	}
	return n
}

func trackTimeRange(t *parser.Track) (first, lastStart, end int64, ok bool) {
	if len(t.Samples) > 0 {
		first = t.Samples[0].DecodeTime
		last := t.Samples[len(t.Samples)-1]
		lastStart = last.DecodeTime
		end = last.DecodeTime + int64(last.Delta)
		return first, lastStart, end, true
	}
	if len(t.Fragments) > 0 {
		have := false
		var cursor int64
		for _, fr := range t.Fragments {
			base := fr.BaseTime
			pos := base
			for _, sm := range fr.Samples {
				if !have {
					first = pos
					have = true
				}
				lastStart = pos
				pos += int64(sm.Duration)
				end = pos
			}
			cursor = end
		}
		_ = cursor
		return first, lastStart, end, have
	}
	if t.Duration > 0 {
		return 0, int64(t.Duration), int64(t.Duration), true
	}
	return 0, 0, 0, false
}
