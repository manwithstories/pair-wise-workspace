package playlist

import (
	"fmt"
	"math"
	"time"
)

// WindowSegment is a segment retained by a rolling media window.
type WindowSegment struct {
	URI             string
	Duration        time.Duration
	FirstPCR        int64
	LastPCR         int64
	FrameInterval   time.Duration
	Size            int64
	SourceSequence  int64
	Sequence        int64
	Discontinuity   bool
	MeasuredPCRSpan int64
}

// Window maintains media and discontinuity sequence numbers while retaining
// only the newest limit segments. A non-positive limit means unlimited.
type Window struct {
	TargetDuration        time.Duration
	Limit                 int
	MediaSequence         int64
	DiscontinuitySequence int64
	segments              []WindowSegment
}

// NewWindow creates a rolling window with an initial media sequence.
func NewWindow(targetDuration time.Duration, limit int, mediaSequence int64) (*Window, error) {
	if targetDuration <= 0 {
		return nil, fmt.Errorf("target duration must be positive")
	}
	if limit < 0 {
		return nil, fmt.Errorf("window must not be negative")
	}
	return &Window{TargetDuration: targetDuration, Limit: limit, MediaSequence: mediaSequence}, nil
}

// Add appends a chronological segment and rolls the window when necessary.
// The return value reports whether a discontinuity was inserted.
func (w *Window) Add(seg WindowSegment) bool {
	discontinuity := false
	if len(w.segments) > 0 {
		prev := w.segments[len(w.segments)-1]
		gap := ticksToDuration(seg.FirstPCR - prev.LastPCR)
		if gap*2 > w.TargetDuration*3 {
			discontinuity = true
			w.DiscontinuitySequence++
		}
	}
	seg.Discontinuity = discontinuity
	w.segments = append(w.segments, seg)
	for w.Limit > 0 && len(w.segments) > w.Limit {
		w.segments = w.segments[1:]
		w.MediaSequence++
	}
	w.renumber()
	return discontinuity
}

func ticksToDuration(ticks int64) time.Duration {
	return time.Duration(float64(ticks) * float64(time.Second) / 27_000_000)
}

// Segments returns a copy of the current window in playback order.
func (w *Window) Segments() []WindowSegment {
	return append([]WindowSegment(nil), w.segments...)
}

// Len reports the number of retained segments.
func (w *Window) Len() int { return len(w.segments) }

// Playlist renders the window as a media playlist.
func (w *Window) Playlist(endList bool) *MediaPlaylist {
	p := &MediaPlaylist{
		Version:               3,
		TargetDuration:        int(math.Ceil(w.TargetDuration.Seconds())),
		MediaSequence:         w.MediaSequence,
		DiscontinuitySequence: w.DiscontinuitySequence,
		EndList:               endList,
	}
	for _, seg := range w.segments {
		p.Segments = append(p.Segments, MediaSegment{
			URI:           seg.URI,
			Duration:      seg.Duration.Seconds(),
			Discontinuity: seg.Discontinuity,
		})
	}
	return p
}

func (w *Window) renumber() {
	for i := range w.segments {
		w.segments[i].Sequence = w.MediaSequence + int64(i)
	}
}
