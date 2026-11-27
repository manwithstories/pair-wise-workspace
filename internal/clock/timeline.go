package clock

import "fmt"

// TrackSpec describes one track's native timing as discovered by the scanner.
type TrackSpec struct {
	Track    int
	TimeBase Rational
	Start    int64 // first presentation timestamp in TimeBase ticks
	Duration int64 // stream duration in TimeBase ticks, NoTimestamp if unknown
	Origin   int64 // unified tick where this track begins, usually 0
}

// Timeline maps every track onto one unified timeline (Microsecond ticks).
// It validates time bases and durations up front so later stages can assume
// every timestamp is sound.
type Timeline struct {
	tracks map[int]trackClock
	order  []int
	base   Rational
}

type trackClock struct {
	spec   TrackSpec
	origin int64 // unified ticks corresponding to spec.Start
}

// errAt builds a unified error located at a track and time position.
func errAt(code ErrCode, op string, track int, pos int64, tb Rational, detail string) *Error {
	return (&Error{Code: code, Op: op, Detail: detail}).WithPosition(track, pos, tb)
}

// NewTimeline validates the specs and unifies their clocks.
func NewTimeline(tracks []TrackSpec) (*Timeline, error) {
	t := &Timeline{tracks: make(map[int]trackClock, len(tracks)), base: Microsecond}
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
		if s.Start != NoTimestamp && s.Duration != NoTimestamp {
			end, err := AddTicks(s.Start, s.Duration)
			if err != nil || end < s.Start {
				return nil, errAt(CodeTimelineConflict, "timeline.track", s.Track, s.Start, s.TimeBase,
					fmt.Sprintf("end %d before start %d", end, s.Start))
			}
		}
		if s.Start == NoTimestamp {
			// Unknown start is treated as zero; the media core only emits
			// non-negative unified timestamps.
			s.Start = 0
		}
		if s.Origin < 0 {
			return nil, errAt(CodeOutOfRange, "timeline.track", s.Track, s.Origin, s.TimeBase, "negative origin")
		}
		t.tracks[s.Track] = trackClock{spec: s, origin: s.Origin}
		t.order = append(t.order, s.Track)
	}
	return t, nil
}

// End returns the exclusive end tick of the spec.
func (s TrackSpec) End() int64 {
	if s.Duration == NoTimestamp || s.Start == NoTimestamp {
		return NoTimestamp
	}
	return s.Start + s.Duration
}

// Base returns the unified timeline base.
func (t *Timeline) Base() Rational { return t.base }

// Tracks returns track indexes in registration order.
func (t *Timeline) Tracks() []int { return append([]int(nil), t.order...) }

// Duration returns the unified end of a track.
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

// ToUnified converts a track-native timestamp into unified microseconds.
// Negative positions and overflows are reported with track and position
// information rather than being silently clamped.
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
	rel, err := Convert(pts, tc.spec.TimeBase, t.base)
	if err != nil {
		return 0, withTrack(err, track, pts, tc.spec.TimeBase)
	}
	if tc.spec.Start != 0 && pts >= tc.spec.Start {
		off, err := Convert(tc.spec.Start, tc.spec.TimeBase, t.base)
		if err != nil {
			return 0, withTrack(err, track, tc.spec.Start, tc.spec.TimeBase)
		}
		rel -= off
	}
	out, err := AddTicks(rel, tc.origin)
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
	rel := unified
	if tc.spec.Start != 0 || tc.origin != 0 {
		off, err := Convert(tc.spec.Start, tc.spec.TimeBase, t.base)
		if err != nil {
			return 0, withTrack(err, track, tc.spec.Start, tc.spec.TimeBase)
		}
		rel, err = AddTicks(rel, off)
		if err != nil {
			return 0, withTrack(err, track, unified, t.base)
		}
		rel, err = AddTicks(rel, -tc.origin)
		if err != nil {
			return 0, withTrack(err, track, unified, t.base)
		}
	}
	native, err := Convert(rel, t.base, tc.spec.TimeBase)
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
		if d > max {
			max = d
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
