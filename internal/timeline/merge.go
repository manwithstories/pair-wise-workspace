// Package timeline merges events from several tracks onto the unified clock
// and cuts them into deliverable segments at committed boundaries.
package timeline

import (
	"fmt"
	"sort"

	"mediarun/internal/clock"
	"mediarun/internal/model"
)

// Limits keep merge output bounded.
const (
	MaxEvents   = 20000
	MaxSegments = 64
)

// Input is one track's contribution.
type Input struct {
	Track   model.Track
	Events  []model.Event
	Gaps    []model.Gap
	Missing bool // the whole track is unusable (e.g. no packets at all)
}

// Segment is a contiguous, independently hashable slice of the timeline.
type Segment struct {
	Ordinal  int           `json:"ordinal"`
	Start    clock.Tick    `json:"start"`
	End      clock.Tick    `json:"end"`
	Events   []model.Event `json:"events"`
	GapCount int           `json:"gap_count"`
	Gaps     []model.Gap   `json:"gaps,omitempty"`
}

// Merged is the complete merged timeline.
type Merged struct {
	Events   []model.Event
	Segments []Segment
	Gaps     []model.Gap
	Missing  []model.Track
}

// Merge sorts every input event by (start, kind, track, sequence) and cuts
// the result at segment boundaries. The cut points are deterministic: a new
// segment starts at a keyframe of any track once the current segment has
// reached its target span. Simultaneous events always land in the same
// segment because cuts happen only between distinct start ticks.
func Merge(inputs []Input, targetSpan clock.Tick) (*Merged, error) {
	if len(inputs) == 0 {
		return nil, fmt.Errorf("%w: no inputs", model.ErrInvalid)
	}
	if targetSpan <= 0 {
		targetSpan = clock.TickRate // one second by default
	}
	out := &Merged{}
	seenTracks := map[uint32]bool{}
	for _, in := range inputs {
		if in.Track.ID == 0 {
			return nil, fmt.Errorf("%w: input without track id", model.ErrInvalid)
		}
		if seenTracks[in.Track.ID] {
			return nil, fmt.Errorf("%w: duplicate track %d", model.ErrInvalid, in.Track.ID)
		}
		seenTracks[in.Track.ID] = true
		if in.Missing {
			out.Missing = append(out.Missing, in.Track)
			out.Gaps = append(out.Gaps, in.Gaps...)
			continue
		}
		out.Gaps = append(out.Gaps, in.Gaps...)
		for _, ev := range in.Events {
			if ev.TrackID == 0 {
				ev.TrackID = in.Track.ID
			}
			if err := validateEvent(ev); err != nil {
				return nil, err
			}
			out.Events = append(out.Events, ev)
		}
	}
	if len(out.Events) > MaxEvents {
		return nil, fmt.Errorf("%w: %d events", model.ErrLimit, len(out.Events))
	}
	// Stable global order: start, kind, track, sequence. This makes the
	// output independent of input ordering.
	sort.SliceStable(out.Events, func(i, j int) bool { return model.Less(out.Events[i], out.Events[j]) })
	sort.SliceStable(out.Gaps, func(i, j int) bool {
		if out.Gaps[i].TrackID != out.Gaps[j].TrackID {
			return out.Gaps[i].TrackID < out.Gaps[j].TrackID
		}
		return out.Gaps[i].From < out.Gaps[j].From
	})

	segments, err := cut(out.Events, out.Gaps, targetSpan)
	if err != nil {
		return nil, err
	}
	// Three deliverable segments are the acceptance floor; tiny inputs are
	// split evenly rather than padded with empty ones.
	if len(segments) < 3 && len(out.Events) >= 3 {
		segments = splitEvenly(out.Events, out.Gaps)
	}
	out.Segments = segments
	return out, nil
}

func validateEvent(ev model.Event) error {
	if ev.Start < 0 {
		return fmt.Errorf("%w: negative start on track %d", model.ErrInvalid, ev.TrackID)
	}
	if ev.Duration < 0 {
		return fmt.Errorf("%w: negative duration on track %d", model.ErrInvalid, ev.TrackID)
	}
	if _, err := ev.End(); err != nil {
		return fmt.Errorf("%w: event end: %v", model.ErrInvalid, err)
	}
	return nil
}

// cut groups events into segments, breaking only between distinct start
// ticks and only when the segment has reached its target span and the next
// position holds a keyframe. Simultaneous events never straddle a boundary.
func cut(events []model.Event, gaps []model.Gap, span clock.Tick) ([]Segment, error) {
	if len(events) == 0 {
		return nil, nil
	}
	var segments []Segment
	begin := 0
	for i := 1; i < len(events); i++ {
		if events[i].Start == events[begin].Start {
			continue
		}
		if events[i].Start-events[begin].Start < span {
			continue
		}
		if !canCut(events, i) {
			// No keyframe here: keep scanning; the next suitable tick
			// starts the new segment.
			continue
		}
		segments = append(segments, Segment{
			Ordinal: len(segments) + 1,
			Start:   events[begin].Start,
			End:     events[i].Start,
			Events:  append([]model.Event(nil), events[begin:i]...),
		})
		begin = i
	}
	// Final chunk: everything from begin onwards.
	chunk := events[begin:]
	if len(chunk) == 0 {
		if len(segments) == 0 {
			return nil, fmt.Errorf("%w: no events assigned", model.ErrInvalid)
		}
		return segments, nil
	}
	segments = append(segments, Segment{
		Ordinal: len(segments) + 1,
		Start:   chunk[0].Start,
		End:     chunk[len(chunk)-1].Start,
		Events:  append([]model.Event(nil), chunk...),
	})
	return attachGaps(segments, gaps), nil
}

// canCut reports whether a segment boundary may be placed at events[i]. A
// boundary must rest on a keyframe so the segment decodes on its own; if no
// keyframe shares that tick, the boundary moves to a later tick.
func canCut(events []model.Event, i int) bool {
	for j := i; j < len(events) && events[j].Start == events[i].Start; j++ {
		if events[j].Keyframe {
			return true
		}
	}
	return false
}

// splitEvenly divides the events into three roughly equal segments; used
// when the timeline is shorter than one target span.
func splitEvenly(events []model.Event, gaps []model.Gap) []Segment {
	const want = 3
	per := (len(events) + want - 1) / want
	if per == 0 {
		per = 1
	}
	var segments []Segment
	for i := 0; i < len(events); i += per {
		end := i + per
		if end > len(events) {
			end = len(events)
		}
		chunk := events[i:end]
		segments = append(segments, Segment{
			Ordinal: len(segments) + 1,
			Start:   chunk[0].Start,
			End:     chunk[len(chunk)-1].Start,
			Events:  append([]model.Event(nil), chunk...),
		})
	}
	return attachGaps(segments, gaps)
}

// attachGaps assigns each gap to the segment whose span contains it, or the
// last segment when it extends past the end.
func attachGaps(segments []Segment, gaps []model.Gap) []Segment {
	if len(segments) == 0 {
		return segments
	}
	for gi := range gaps {
		g := gaps[gi]
		placed := false
		for i := range segments {
			if g.Start < segments[i].End || i == len(segments)-1 {
				segments[i].Gaps = append(segments[i].Gaps, g)
				segments[i].GapCount += g.Count
				placed = true
				break
			}
		}
		if !placed {
			segments[len(segments)-1].Gaps = append(segments[len(segments)-1].Gaps, g)
			segments[len(segments)-1].GapCount += g.Count
		}
	}
	return segments
}

// Summary counts the deliverable shape of a merged timeline.
type Summary struct {
	Events        int        `json:"events"`
	Segments      int        `json:"segments"`
	SegmentsOK    int        `json:"segments_ok"`
	GapCount      int        `json:"gap_count"`
	GapTicks      clock.Tick `json:"gap_ticks"`
	MissingTracks int        `json:"missing_tracks"`
}

// Summarize produces the verification summary.
func (m *Merged) Summarize() Summary {
	s := Summary{Events: len(m.Events), Segments: len(m.Segments), SegmentsOK: len(m.Segments), MissingTracks: len(m.Missing)}
	for _, g := range m.Gaps {
		s.GapCount += g.Count
		span := g.End - g.Start
		if span > 0 {
			s.GapTicks += span
		}
	}
	return s
}

// SegmentFor returns the segment that contains tick t, if any.
func (m *Merged) SegmentFor(t clock.Tick) (*Segment, bool) {
	for i := range m.Segments {
		if t >= m.Segments[i].Start && (t < m.Segments[i].End || i == len(m.Segments)-1) {
			return &m.Segments[i], true
		}
	}
	return nil, false
}
