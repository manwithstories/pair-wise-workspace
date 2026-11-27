package clock

import "fmt"

// TrackSpec describes one track's native timing as discovered by the scanner.
type TrackSpec struct {
	Track    int
	TimeBase Rational
	Start    int64 // first presentation timestamp in TimeBase ticks
	Duration int64 // stream duration in TimeBase ticks, NoTimestamp if unknown
	Origin   int64 // extra unified offset for this track, usually 0
}

// Timeline maps every track onto one unified timeline (Microsecond ticks).
//
// All tracks share a single container origin: the earliest track start in
// unified ticks. A subtitle cue that begins 200 ms after the audio therefore
// keeps its 200 ms position instead of being rebased to zero. Using one origin
// (rather than a per-track origin) is what lets audio, subtitles, chapters and
// timed metadata be compared with each other.
type Timeline struct {
	tracks map[int]trackClock
	order  []int
	base   Rational
	origin int64 // unified ticks of the shared container origin
}

type trackClock struct {
	spec TrackSpec
}

// errAt builds a unified error located at a track and time position.
func errAt(code ErrCode, op string, track int, pos int64, tb Rational, detail string) *Error {
	return (&Error{Code: code, Op: op, Detail: detail}).WithPosition(track, pos, tb)
}

// NewTimeline validates the specs, picks the shared origin and unifies clocks.
func NewTimeline(tracks []TrackSpec) (*Timeline, error) {
	t := &Timeline{tracks: make(map[int]trackClock, len(tracks)), base: Microsecond, origin: -1}
	for _, s := range tracks {
		if s.Track < 0 {
			return nil, errAt(CodeOutOfRange, "timeline.track", s.Track, s.Start, s.TimeBase, "track index must be non-negative")
		}
		if _, dup := t.tracks[s.Track]; dup {
			return nil, errAt(CodeInvalidTimestamp, "timeline.track", s.Track, s.Start, s.TimeBase, fmt.Sprintf("duplicate track %d", s.Track))
		}
		if !s.TimeBase.Valid() {
			return nil, errAt(CodeInvalidTimeBase, "timeline.track", s.Track, s.Start, s.TimeBase, "invalid time base")
		}
		if s.Duration != NoTimestamp && s.Duration < 0 {
			return nil, errAt(CodeNegativeDuration, "timeline.track", s.Track, s.Duration, s.TimeBase, "negative duration")
		}
		if s.Start == NoTimestamp {
			s.Start = 0
		}
		if s.Start < 0 {
			// A negative first timestamp is corrupt input; refuse rather than
			// silently clamp, so the caller can locate the record.
			return nil, errAt(CodeInvalidTimestamp, "timeline.track", s.Track, s.Start, s.TimeBase, "negative start timestamp")
		}
		if s.Origin < 0 {
			return nil, errAt(CodeOutOfRange, "timeline.track", s.Track, s.Origin, s.TimeBase, "negative origin")
		}
		startUS, err := Convert(s.Start, s.TimeBase, t.base)
		if err != nil {
			return nil, withTrack(err, s.Track, s.Start, s.TimeBase)
		}
		if t.origin < 0 || startUS < t.origin {
			t.origin = startUS
		}
		t.tracks[s.Track] = trackClock{spec: s}
		t.order = append(t.order, s.Track)
	}
	if t.origin < 0 {
		t.origin = 0
	}
	return t, nil
}

// Base returns the unified timeline base.
func (t *Timeline) Base() Rational { return t.base }

// Origin returns the shared container origin in unified ticks.
func (t *Timeline) Origin() int64 { return t.origin }

// Tracks returns track indexes in registration order.
func (t *Timeline) Tracks() []int { return append([]int(nil), t.order...) }

// Duration returns the end of a track in unified ticks, measured from origin.
func (t *Timeline) Duration(track int) (int64, error) {
	tc, ok := t.tracks[track]
	if !ok {
		return 0, &Error{Code: CodeOutOfRange, Op: "timeline.duration", Track: track, Detail: "unknown track"}
	}
	if tc.spec.Duration == NoTimestamp {
		return NoTimestamp, nil
	}
	d, err := Convert(tc.spec.Duration, tc.spec.TimeBase, t.base)
	if err != nil {
		return 0, withTrack(err, track, tc.spec.Duration, tc.spec.TimeBase)
	}
	return d, nil
}

// ToUnified converts a track-native timestamp into unified microseconds
// relative to the shared container origin.
func (t *Timeline) ToUnified(track int, pts int64) (int64, error) {
	tc, ok := t.tracks[track]
	if !ok {
		return 0, &Error{Code: CodeOutOfRange, Op: "timeline.to_unified", Track: track, Detail: "unknown track"}
	}
	if pts == NoTimestamp {
		return NoTimestamp, nil
	}
	if pts < 0 {
		return 0, errAt(CodeInvalidTimestamp, "timeline.to_unified", track, pts, tc.spec.TimeBase, "negative presentation timestamp")
	}
	abs, err := Convert(pts, tc.spec.TimeBase, t.base)
	if err != nil {
		return 0, withTrack(err, track, pts, tc.spec.TimeBase)
	}
	rel, err := AddTicks(abs, -t.origin)
	if err != nil {
		return 0, withTrack(err, track, pts, tc.spec.TimeBase)
	}
	out, err := AddTicks(rel, tc.spec.Origin)
	if err != nil {
		return 0, withTrack(err, track, pts, tc.spec.TimeBase)
	}
	if out < 0 {
		return 0, errAt(CodeOutOfRange, "timeline.to_unified", track, pts, tc.spec.TimeBase, "unified timestamp is negative")
	}
	return out, nil
}

// ToTrack converts a unified microsecond timestamp back to the track's native
// time base, e.g. to write an interleaved result with original packets.
func (t *Timeline) ToTrack(track int, unified int64) (int64, error) {
	tc, ok := t.tracks[track]
	if !ok {
		return 0, &Error{Code: CodeOutOfRange, Op: "timeline.to_track", Track: track, Detail: "unknown track"}
	}
	if unified == NoTimestamp {
		return NoTimestamp, nil
	}
	if unified < 0 {
		return 0, errAt(CodeInvalidTimestamp, "timeline.to_track", track, unified, t.base, "negative unified timestamp")
	}
	rel, err := AddTicks(unified, -tc.spec.Origin)
	if err != nil {
		return 0, withTrack(err, track, unified, t.base)
	}
	abs, err := AddTicks(rel, t.origin)
	if err != nil {
		return 0, withTrack(err, track, unified, t.base)
	}
	native, err := Convert(abs, t.base, tc.spec.TimeBase)
	if err != nil {
		return 0, withTrack(err, track, unified, t.base)
	}
	return native, nil
}

// TotalDuration returns the longest track duration in unified ticks.
func (t *Timeline) TotalDuration() (int64, error) {
	var max int64 = -1
	for _, idx := range t.order {
		d, err := t.Duration(idx)
		if err != nil {
			return 0, err
		}
		if d == NoTimestamp {
			continue
		}
		// Add the track start relative to origin so the total covers the
		// whole container rather than just the longest single stream.
		tc := t.tracks[idx]
		startUS, err := Convert(tc.spec.Start, tc.spec.TimeBase, t.base)
		if err != nil {
			return 0, withTrack(err, idx, tc.spec.Start, tc.spec.TimeBase)
		}
		span, err := AddTicks(d, startUS-t.origin)
		if err != nil {
			return 0, withTrack(err, idx, tc.spec.Start, tc.spec.TimeBase)
		}
		if span > max {
			max = span
		}
	}
	if max < 0 {
		return 0, &Error{Code: CodeInvalidTimestamp, Op: "timeline.total_duration", Detail: "no timed tracks"}
	}
	return max, nil
}

func withTrack(err error, track int, pos int64, tb Rational) error {
	if e, ok := AsError(err); ok {
		return e.WithPosition(track, pos, tb)
	}
	return &Error{Code: CodeInvalidTimestamp, Op: "timeline", Track: track, Position: pos, TimeBase: tb, Err: err}
}

// MergeIntervals validates a list of [start,end) spans in unified ticks and
// returns the covered duration. Overlapping or inverted spans are rejected as
// timeline conflicts, proving metadata never leaves the audio timeline.
func MergeIntervals(spans [][2]int64) (int64, error) {
	var total int64
	var prevEnd int64 = -1
	for i, s := range spans {
		if s[0] < 0 || s[1] < 0 {
			return 0, &Error{Code: CodeOutOfRange, Op: "merge_intervals", Detail: fmt.Sprintf("span %d negative", i)}
		}
		if s[1] < s[0] {
			return 0, &Error{Code: CodeNegativeDuration, Op: "merge_intervals", Detail: fmt.Sprintf("span %d [%d,%d)", i, s[0], s[1])}
		}
		if s[0] < prevEnd {
			return 0, &Error{Code: CodeTimelineConflict, Op: "merge_intervals", Position: s[0], Detail: fmt.Sprintf("span %d overlaps previous", i)}
		}
		prevEnd = s[1]
		total += s[1] - s[0]
	}
	return total, nil
}
