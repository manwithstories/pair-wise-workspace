// Package export builds the single analysis shared by inspect, seek and
// export: one parse, one index, one set of timelines — so every output
// format reports identical fields.
package export

import (
	"fmt"
	"os"

	"mp4loc/internal/box"
	"mp4loc/internal/index"
	"mp4loc/internal/time"
)

// TrackReport is the per-track summary shared by all output formats.
type TrackReport struct {
	ID               uint32  `json:"id"`
	Handler          string  `json:"handler"`
	Timescale        uint32  `json:"timescale"`
	SampleCount      int     `json:"sample_count"`
	KeyframeCount    int     `json:"keyframe_count"`
	HasSTSS          bool    `json:"has_stss"`
	EditSegmentCount int     `json:"edit_segment_count"`
	WrapEvents       int     `json:"wrap_events"`
	DurationSeconds  float64 `json:"duration_seconds"`
}

// Report is the file-level summary.
type Report struct {
	Path           string        `json:"path"`
	BoxCount       int           `json:"box_count"`
	TrackCount     int           `json:"track_count"`
	VideoTrackID   uint32        `json:"video_track_id,omitempty"`
	MovieTimescale uint32        `json:"movie_timescale"`
	Tracks         []TrackReport `json:"tracks"`
}

// Analysis is the full parsed+indexed view of one file.
type Analysis struct {
	Report    Report
	Movie     *index.Movie
	Timelines map[uint32]*timeline.Timeline // by track ID
	Forest    *box.Forest
	File      *os.File // kept open for sample reads; caller closes
	FileSize  int64
}

// Close releases the underlying file.
func (a *Analysis) Close() {
	if a.File != nil {
		a.File.Close()
	}
}

// Analyze parses and indexes path once. Structural problems surface as
// *box.StructureError; missing/unreadable files as plain errors.
func Analyze(path string) (*Analysis, error) {
	f, size, err := box.OpenBounded(path, 0)
	if err != nil {
		return nil, err
	}
	forest, err := box.Parse(f, size, nil)
	if err != nil {
		f.Close()
		return nil, err
	}
	movie, err := index.Build(f, forest)
	if err != nil {
		f.Close()
		return nil, err
	}
	a := &Analysis{
		Movie:     movie,
		Timelines: map[uint32]*timeline.Timeline{},
		Forest:    forest,
		File:      f,
		FileSize:  size,
	}
	rep := Report{
		Path:           path,
		BoxCount:       forest.Count(),
		TrackCount:     len(movie.Tracks),
		MovieTimescale: movie.Timescale,
	}
	for _, tr := range movie.Tracks {
		tl := timeline.Build(movie.Timescale, tr)
		a.Timelines[tr.ID] = tl
		keyframes := len(tr.Table.Keyframes)
		rep.Tracks = append(rep.Tracks, TrackReport{
			ID:               tr.ID,
			Handler:          tr.Handler,
			Timescale:        tr.Timescale,
			SampleCount:      tr.Table.SampleCount(),
			KeyframeCount:    keyframes,
			HasSTSS:          tr.Table.HasSTSS,
			EditSegmentCount: len(tr.Edits),
			WrapEvents:       len(tl.WrapEvents),
			DurationSeconds:  float64(tl.Duration) / float64(tl.MovieTimescale),
		})
	}
	if vt := movie.VideoTrack(); vt != nil {
		rep.VideoTrackID = vt.ID
	}
	a.Report = rep
	return a, nil
}

// Text renders the inspect text summary. The primary (video, else first)
// track's fields are emitted as bare keys; every track also gets a
// prefixed section.
func (r *Report) Text() string {
	var b []byte
	put := func(format string, args ...interface{}) {
		b = append(b, fmt.Sprintf(format+"\n", args...)...)
	}
	put("BOX_COUNT: %d", r.BoxCount)
	put("TRACK_COUNT: %d", r.TrackCount)
	put("MOVIE_TIMESCALE: %d", r.MovieTimescale)
	if r.VideoTrackID != 0 {
		put("VIDEO_TRACK_ID: %d", r.VideoTrackID)
	}
	primary := -1
	for i, t := range r.Tracks {
		if t.ID == r.VideoTrackID {
			primary = i
		}
	}
	if primary < 0 && len(r.Tracks) > 0 {
		primary = 0
	}
	if primary >= 0 {
		t := r.Tracks[primary]
		put("TIMESCALE: %d", t.Timescale)
		put("SAMPLE_COUNT: %d", t.SampleCount)
		put("KEYFRAME_COUNT: %d", t.KeyframeCount)
		put("EDIT_SEGMENT_COUNT: %d", t.EditSegmentCount)
		put("WRAP_EVENTS: %d", t.WrapEvents)
	}
	for _, t := range r.Tracks {
		put("TRACK[%d].HANDLER: %s", t.ID, t.Handler)
		put("TRACK[%d].TIMESCALE: %d", t.ID, t.Timescale)
		put("TRACK[%d].SAMPLE_COUNT: %d", t.ID, t.SampleCount)
		put("TRACK[%d].KEYFRAME_COUNT: %d", t.ID, t.KeyframeCount)
		put("TRACK[%d].HAS_STSS: %t", t.ID, t.HasSTSS)
		put("TRACK[%d].EDIT_SEGMENT_COUNT: %d", t.ID, t.EditSegmentCount)
		put("TRACK[%d].WRAP_EVENTS: %d", t.ID, t.WrapEvents)
		put("TRACK[%d].DURATION_SECONDS: %.3f", t.ID, t.DurationSeconds)
	}
	return string(b)
}
