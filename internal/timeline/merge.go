package timeline

import "sort"

// Source records where an event's timestamp came from.
type Source uint8

const (
	// SourceExplicit timestamps were carried by the block itself.
	SourceExplicit Source = iota
	// SourceInferred timestamps were derived from the previous block of
	// the same track (previous start + previous duration).
	SourceInferred
)

// Event is one media block on the unified timeline.
type Event struct {
	Track uint64
	Start Nanos
	Dur   Nanos
	Seq   int // ingestion order, for stable sorting
	Src   Source
	Bytes int
}

// ErrorInterval marks a time range that could not be recovered.
type ErrorInterval struct {
	Track  uint64
	Start  Nanos
	End    Nanos
	Reason string
}

// Merger ingests per-track blocks and produces a single ordered event
// stream. Backwards timestamps are isolated as error events and never
// silently reordered.
type Merger struct {
	scale  Scale
	last   map[uint64]Event
	seq    int
	events []Event
	errs   []ErrorInterval
}

// NewMerger returns a Merger converting ticks with the given scale.
func NewMerger(s Scale) *Merger {
	return &Merger{scale: s, last: make(map[uint64]Event)}
}

// Add ingests one block. A nil ticks means the timestamp is missing and
// is inferred from the previous block of the same track only.
func (m *Merger) Add(track uint64, ticks *int64, durTicks uint64, bytes int) {
	dur := m.scale.ToNanos(int64(durTicks))
	prev, ok := m.last[track]
	var start Nanos
	src := SourceExplicit
	if ticks == nil {
		if !ok {
			m.errs = append(m.errs, ErrorInterval{
				Track:  track,
				Reason: "missing timestamp with no previous block on track",
			})
			return
		}
		start = prev.Start + prev.Dur
		src = SourceInferred
	} else {
		start = m.scale.ToNanos(*ticks)
		if ok && start < prev.Start {
			// Backwards timestamp: isolate as an error event, do not
			// reorder and do not update the track cursor.
			m.errs = append(m.errs, ErrorInterval{
				Track:  track,
				Start:  start,
				End:    start + dur,
				Reason: "backwards timestamp isolated",
			})
			return
		}
	}
	ev := Event{Track: track, Start: start, Dur: dur, Seq: m.seq, Src: src, Bytes: bytes}
	m.seq++
	m.events = append(m.events, ev)
	m.last[track] = ev
}

// Events returns the merged events sorted by (start, track, ingestion
// order) with a stable sort.
func (m *Merger) Events() []Event {
	out := make([]Event, len(m.events))
	copy(out, m.events)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		if out[i].Track != out[j].Track {
			return out[i].Track < out[j].Track
		}
		return out[i].Seq < out[j].Seq
	})
	return out
}

// Errors returns the isolated error intervals.
func (m *Merger) Errors() []ErrorInterval {
	out := make([]ErrorInterval, len(m.errs))
	copy(out, m.errs)
	return out
}

// Extent returns the end of the latest event on the timeline.
func (m *Merger) Extent() Nanos {
	var end Nanos
	for _, e := range m.events {
		if e.Start+e.Dur > end {
			end = e.Start + e.Dur
		}
	}
	return end
}
