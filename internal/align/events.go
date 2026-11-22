// Package align projects cross-track timed events (subtitles, chapters,
// ID3, emsg, SEI) onto the unified timeline and aligns segment boundaries
// across rendition layers.
package align

import (
	"sort"

	"mediaroute/internal/adapter"
	"mediaroute/internal/fault"
)

// Validate checks per-track monotonicity and non-negative durations,
// reporting track, sequence number and raw time for every violation.
// Invalid events are excluded from the returned slice.
func Validate(events []adapter.Event) ([]adapter.Event, []*fault.Error) {
	last := make(map[string]int64)
	var out []adapter.Event
	var errs []*fault.Error
	for _, ev := range events {
		if ev.DurationNs < 0 {
			errs = append(errs, fault.Newf(fault.CatTiming, "negative duration %d", ev.DurationNs).
				WithTrack(ev.Track).WithSeq(ev.Seq).WithRawTime(ev.TimestampNs))
			continue
		}
		if prev, ok := last[ev.Track]; ok && ev.TimestampNs < prev {
			errs = append(errs, fault.Newf(fault.CatTiming,
				"non-monotonic event: %d < %d", ev.TimestampNs, prev).
				WithTrack(ev.Track).WithSeq(ev.Seq).WithRawTime(ev.TimestampNs))
			continue
		}
		last[ev.Track] = ev.TimestampNs
		out = append(out, ev)
	}
	return out, errs
}

// Assign returns the index of the half-open segment [bounds[i], bounds[i+1])
// containing ts, or -1 when ts lies outside. An event exactly on a boundary
// belongs to exactly one segment: the one starting at that boundary.
func Assign(bounds []int64, ts int64) int {
	if len(bounds) < 2 || ts < bounds[0] || ts >= bounds[len(bounds)-1] {
		return -1
	}
	i := sort.Search(len(bounds), func(i int) bool { return bounds[i] > ts }) - 1
	return i
}

// Project assigns every event to its segment. Events outside all segments
// are dropped from the result but counted.
func Project(events []adapter.Event, bounds []int64) (segments [][]adapter.Event, dropped int) {
	segments = make([][]adapter.Event, len(bounds)-1)
	for _, ev := range events {
		i := Assign(bounds, ev.TimestampNs)
		if i < 0 {
			dropped++
			continue
		}
		segments[i] = append(segments[i], ev)
	}
	return segments, dropped
}
