// Package graph turns an explicit multi-track timeline into a validated
// directed acyclic graph and a deterministic processing plan.
package graph

import (
	"errors"

	"mediasim/internal/clock"
)

// TrackKind is the media role of a track.
type TrackKind string

const (
	TrackVideo  TrackKind = "video"
	TrackAudio  TrackKind = "audio"
	TrackMarker TrackKind = "marker"
)

// TransitionKind names the bridge between two segments.
type TransitionKind string

const (
	TransCut       TransitionKind = "cut"
	TransCrossfade TransitionKind = "crossfade"
	TransSlide     TransitionKind = "slide"
	TransFlash     TransitionKind = "flash"
)

// Hard limits keep a hostile timeline from exhausting memory. Every limit is
// reported as a classified argument error instead of a panic.
const (
	MaxTracks      = 8
	MaxSegments    = 64
	MaxEditPoints  = 32
	MaxTransitions = 16
)

// Segment is one deterministic piece of a track, in timeline ticks.
type Segment struct {
	ID         string
	Track      string
	Start      int64
	Duration   int64
	Source     string
	EditPoints []int64  // offsets strictly inside the segment where a cut occurs
	Requires   []string // explicit cross-segment dependencies
}

// Interval returns the half-open tick range of the segment.
func (s Segment) Interval() clock.Interval {
	return clock.Interval{Start: s.Start, End: s.Start + s.Duration}
}

// Track is an ordered group of segments sharing a media role.
type Track struct {
	ID       string
	Kind     TrackKind
	Segments []Segment
}

// Transition links two segments, optionally overlapping them by Duration.
type Transition struct {
	ID       string
	From     string
	To       string
	Kind     TransitionKind
	Duration int64
}

// Timeline is the validated, explicit input to the plan builder.
type Timeline struct {
	Base        clock.Timeline
	Tracks      []Track
	Transitions []Transition
}

func span(seg Segment) clock.Span {
	return clock.NewSpan(seg.Track, seg.ID, seg.Interval())
}

func invalid(cat clock.Category, sp clock.Span, format string, args ...any) *clock.Error {
	return clock.Errorf(cat, sp, clock.RecoveredNone, format, args...)
}

// New validates and freezes a timeline. All problems are returned together
// (joined), each classified, so a reviewer sees the full picture at once.
func New(base clock.Timeline, tracks []Track, transitions []Transition) (*Timeline, error) {
	if !base.Tick.Rate.Positive() || !base.FPS.Positive() {
		return nil, invalid(clock.CatTimebase, clock.Span{}, "non-positive base rate")
	}
	t := &Timeline{Base: base, Tracks: tracks, Transitions: transitions}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	// Freeze: own copies so callers cannot mutate validated input.
	t.Tracks = make([]Track, len(tracks))
	for i, tr := range tracks {
		cp := tr
		cp.Segments = append([]Segment(nil), tr.Segments...)
		for j := range cp.Segments {
			cp.Segments[j].EditPoints = append([]int64(nil), tr.Segments[j].EditPoints...)
			cp.Segments[j].Requires = append([]string(nil), tr.Segments[j].Requires...)
		}
		t.Tracks[i] = cp
	}
	t.Transitions = append([]Transition(nil), transitions...)
	return t, nil
}

// Validate checks counts, identifiers, durations, edit points, implicit
// overlaps and transition references. It mutates only the Track back-pointer.
func (t *Timeline) Validate() error {
	var errs []error
	fail := func(err error) { errs = append(errs, err) }

	if len(t.Tracks) == 0 || len(t.Tracks) > MaxTracks {
		fail(invalid(clock.CatArgument, clock.Span{}, "track count %d outside 1..%d", len(t.Tracks), MaxTracks))
	}
	segByID := make(map[string]Segment, len(t.Tracks)*2)
	trackIDs := make(map[string]struct{}, len(t.Tracks))
	total := 0
	for _, tr := range t.Tracks {
		switch {
		case tr.ID == "":
			fail(invalid(clock.CatArgument, clock.Span{}, "track with empty id"))
		case !tr.Kind.valid():
			fail(invalid(clock.CatArgument, clock.NewSpan(tr.ID, "", clock.Interval{}),
				"unknown track kind %q", tr.Kind))
		}
		if _, dup := trackIDs[tr.ID]; dup {
			fail(invalid(clock.CatArgument, clock.NewSpan(tr.ID, "", clock.Interval{}), "duplicate track id"))
		}
		trackIDs[tr.ID] = struct{}{}
		total += len(tr.Segments)
		for i := range tr.Segments {
			seg := &tr.Segments[i]
			seg.Track = tr.ID
			if err := validateSegment(seg, segByID); err != nil {
				fail(err)
			}
		}
	}
	if total > MaxSegments {
		fail(invalid(clock.CatArgument, clock.Span{}, "segment count %d exceeds limit %d", total, MaxSegments))
	}
	if len(t.Transitions) > MaxTransitions {
		fail(invalid(clock.CatArgument, clock.Span{}, "transition count %d exceeds %d",
			len(t.Transitions), MaxTransitions))
	}
	return errors.Join(errs...)
}

// Segment looks up a segment by ID.
func (t *Timeline) Segment(id string) (Segment, bool) {
	for _, tr := range t.Tracks {
		for _, s := range tr.Segments {
			if s.ID == id {
				return s, true
			}
		}
	}
	return Segment{}, false
}

// DurationTicks is the end of the last segment on any track.
func (t *Timeline) DurationTicks() int64 {
	var end int64
	for _, tr := range t.Tracks {
		for _, s := range tr.Segments {
			if e := s.Start + s.Duration; e > end {
				end = e
			}
		}
	}
	return end
}

func (k TrackKind) valid() bool {
	switch k {
	case TrackVideo, TrackAudio, TrackMarker:
		return true
	}
	return false
}

func (k TransitionKind) valid() bool {
	switch k {
	case TransCut, TransCrossfade, TransSlide, TransFlash:
		return true
	}
	return false
}
