// Package metadata merges cross-track timed events (subtitles, chapters,
// ID3/emsg/SEI) onto the normalized microsecond timeline. Truncated payloads
// are isolated: their first offset is recorded and the surrounding safe
// segments remain usable instead of aborting the stream.
package metadata

import (
	"sort"

	"mediaflow/internal/fixture"
	"mediaflow/internal/timeline"
)

// MergedEvent is one timed event on the normalized timeline.
type MergedEvent struct {
	Track      uint32
	Kind       fixture.EventKind
	StartUS    int64
	DurationUS int64
	EndUS      int64
	Offset     int64 // first source byte offset
	Size       int
	Truncated  bool
	Payload    []byte
}

// Segment is a safe, contiguous microsecond range that contains no truncated
// or otherwise isolated event. Segments are the units remux/resume operate on.
type Segment struct {
	Index   int
	StartUS int64
	EndUS   int64
}

// Report captures the merge outcome.
type Report struct {
	Events        []MergedEvent
	Segments      []Segment
	Truncated     []MergedEvent
	TruncatedOffs []int64 // first byte offsets of every truncated payload
}

// Config controls merge behavior.
type Config struct {
	// EventTimebase is the native tick rate of event timestamps (1 ms).
	EventTimebase timeline.Rational
}

// DefaultConfig uses the millisecond event clock shared by fixture events.
func DefaultConfig() Config {
	return Config{EventTimebase: timeline.MustRational(1, 1000)}
}

// Merge normalizes and orders all events, then partitions the timeline into
// safe segments around truncated payloads.
func Merge(events []fixture.Event, spanUS int64, cfg Config) (*Report, error) {
	rep := &Report{}
	rep.Events = make([]MergedEvent, 0, len(events))
	for _, ev := range events {
		startUS, err := timeline.ToStandard(ev.RawTS, cfg.EventTimebase)
		if err != nil {
			return nil, timeline.TrackError("metadata.merge", ev.Track, ev.RawTS,
				cfg.EventTimebase, true, err)
		}
		durUS := ev.Duration // fixture event durations are already microseconds
		endUS := startUS + durUS
		if endUS < startUS {
			return nil, timeline.TrackError("metadata.merge", ev.Track, ev.RawTS,
				cfg.EventTimebase, true,
				timeline.Wrap("metadata.merge", ev.Track, ev.RawTS, cfg.EventTimebase, startUS, true,
					errNegative(ev)))
		}
		me := MergedEvent{
			Track:      ev.Track,
			Kind:       ev.Kind,
			StartUS:    startUS,
			DurationUS: durUS,
			EndUS:      endUS,
			Offset:     0, // filled below from event ordering
			Size:       len(ev.Payload),
			Truncated:  ev.Truncated,
			Payload:    append([]byte(nil), ev.Payload...),
		}
		rep.Events = append(rep.Events, me)
	}

	// Assign deterministic offsets in source order and then sort by time.
	var offset int64
	for i := range rep.Events {
		rep.Events[i].Offset = offset
		offset += int64(rep.Events[i].Size)
	}
	sort.SliceStable(rep.Events, func(i, j int) bool {
		if rep.Events[i].StartUS != rep.Events[j].StartUS {
			return rep.Events[i].StartUS < rep.Events[j].StartUS
		}
		if rep.Events[i].Track != rep.Events[j].Track {
			return rep.Events[i].Track < rep.Events[j].Track
		}
		return rep.Events[i].Kind < rep.Events[j].Kind
	})

	for _, ev := range rep.Events {
		if ev.Truncated {
			rep.Truncated = append(rep.Truncated, ev)
			rep.TruncatedOffs = append(rep.TruncatedOffs, ev.Offset)
		}
	}
	rep.Segments = buildSegments(rep.Events, spanUS)
	return rep, nil
}

// buildSegments returns safe half-open ranges excluding each truncated event's
// own [Start, End) span. Adjacent safe ranges are kept separate so a consumer
// can resume at the nearest complete boundary.
func buildSegments(events []MergedEvent, spanUS int64) []Segment {
	var segments []Segment
	cursor := int64(0)
	idx := 0
	flush := func(end int64) {
		if end <= cursor {
			return
		}
		segments = append(segments, Segment{Index: idx, StartUS: cursor, EndUS: end})
		idx++
	}
	for _, ev := range events {
		if !ev.Truncated {
			continue
		}
		// Safe region up to the truncated event start.
		flush(ev.StartUS)
		// Skip the isolated region and resume after it ends.
		if ev.EndUS > cursor {
			cursor = ev.EndUS
		}
	}
	flush(spanUS)
	return segments
}

// FindByKind returns merged events of one kind in time order.
func (r *Report) FindByKind(kind fixture.EventKind) []MergedEvent {
	var out []MergedEvent
	for _, ev := range r.Events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// IsSafe reports whether a normalized timestamp lies inside a safe segment.
func (r *Report) IsSafe(t int64) bool {
	for _, s := range r.Segments {
		if t >= s.StartUS && t < s.EndUS {
			return true
		}
	}
	return false
}

func errNegative(ev fixture.Event) error {
	return &timeline.Error{
		Op: "metadata.merge", Track: ev.Track, RawTS: ev.RawTS,
		AtMicros: ev.TrueUS, Recoverable: true, Err: timeline.ErrNegativeDuration,
	}
}
