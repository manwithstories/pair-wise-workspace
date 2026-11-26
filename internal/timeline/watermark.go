// Package timeline orders normalised events, advances an event-time watermark
// and deterministically replays the affected suffix when a late event lands.
package timeline

import (
	"sort"

	"mediad/internal/media"
)

// LatenessWindow is the production late-arrival window: 2 seconds.
const LatenessWindow = media.MediaTick(2 * media.TickRate)

// Event is one stable, sortable unit of work on the delivery timeline. Time is
// on the shared host timeline; In/Out stay in the source track's raw ticks so
// audio can be cut sample-accurately.
type Event struct {
	Time   media.MediaTick `json:"time"`
	Camera string          `json:"camera"`
	Track  string          `json:"track"`
	Seq    int64           `json:"seq"`
	In     int64           `json:"in"`
	Out    int64           `json:"out"`
	Kind   string          `json:"kind"`
}

// Compare implements the canonical order: time, camera, track, seq. It is a
// total order over the event identity fields, so sorting is deterministic for
// any arrival sequence.
func Compare(a, b Event) int {
	if a.Time != b.Time {
		if a.Time < b.Time {
			return -1
		}
		return 1
	}
	if a.Camera != b.Camera {
		if a.Camera < b.Camera {
			return -1
		}
		return 1
	}
	if a.Track != b.Track {
		if a.Track < b.Track {
			return -1
		}
		return 1
	}
	switch {
	case a.Seq < b.Seq:
		return -1
	case a.Seq > b.Seq:
		return 1
	}
	return 0
}

// Timeline is the watermark-driven event store. It is driven from a single
// goroutine and is not safe for concurrent use.
type Timeline struct {
	lateness      media.MediaTick
	snapshotEvery int

	started   bool
	watermark media.MediaTick
	events    []Event // canonical sorted order
	arrivals  int

	snapshots []Snapshot

	lateEvents     int
	replayedEvents int
	maxLateness    media.MediaTick
	lastReplayFrom media.MediaTick
	lastReplayTo   media.MediaTick
}

// New creates a timeline with the given late window and snapshot stride. A
// stride <= 0 falls back to 4.
func New(lateness media.MediaTick, snapshotEvery int) *Timeline {
	if lateness < 0 {
		lateness = 0
	}
	if snapshotEvery <= 0 {
		snapshotEvery = 4
	}
	return &Timeline{lateness: lateness, snapshotEvery: snapshotEvery}
}

// NewDefault creates a timeline with the 2 second production late window.
func NewDefault() *Timeline { return New(LatenessWindow, 4) }

// Watermark returns the current event-time watermark.
func (t *Timeline) Watermark() media.MediaTick { return t.watermark }

// LateEvents reports how many events arrived outside the late window.
func (t *Timeline) LateEvents() int { return t.lateEvents }

// ReplayedEvents reports how many already-accepted events were recomputed
// because of late arrivals.
func (t *Timeline) ReplayedEvents() int { return t.replayedEvents }

// MaxLateness reports the worst late arrival seen, in ticks.
func (t *Timeline) MaxLateness() media.MediaTick { return t.maxLateness }

// LastReplayInterval reports the media-time range recomputed by the most recent
// replay, which the pipeline uses to invalidate exactly the affected outputs.
func (t *Timeline) LastReplayInterval() (media.MediaTick, media.MediaTick) {
	return t.lastReplayFrom, t.lastReplayTo
}

// Len reports the accepted event count.
func (t *Timeline) Len() int { return len(t.events) }

// Events returns the accepted events in canonical order.
func (t *Timeline) Events() []Event {
	out := make([]Event, len(t.events))
	copy(out, t.events)
	return out
}

// Add inserts an event, returning the number of already-accepted events that
// had to be recomputed (zero for on-time arrivals).
//
// An event is late when its timestamp falls more than the window behind the
// current watermark. Late events never rewind the watermark; the suffix from
// the nearest snapshot is rebuilt in canonical order, which makes the final
// event list identical to a run where no event was late.
func (t *Timeline) Add(e Event) int {
	t.arrivals++
	if !t.started {
		t.started = true
		t.events = []Event{e}
		t.watermark = e.Time
		t.maybeSnapshot()
		return 0
	}

	late := e.Time+t.lateness < t.watermark
	if late {
		replayed := t.replay(e)
		t.lateEvents++
		if lag := t.watermark - e.Time; lag > t.maxLateness {
			t.maxLateness = lag
		}
		t.replayedEvents += replayed
		return replayed
	}

	t.insert(e)
	if e.Time > t.watermark {
		t.watermark = e.Time
	}
	t.maybeSnapshot()
	return 0
}

// Finish closes the stream by advancing the watermark to the newest accepted
// event, so a post-roll late event is judged against the closed window.
func (t *Timeline) Finish() {
	for _, e := range t.events {
		if e.Time > t.watermark {
			t.watermark = e.Time
		}
	}
}

func (t *Timeline) insert(e Event) {
	idx := sort.Search(len(t.events), func(i int) bool { return Compare(t.events[i], e) > 0 })
	t.events = append(t.events, Event{})
	copy(t.events[idx+1:], t.events[idx:])
	t.events[idx] = e
}

func (t *Timeline) maybeSnapshot() {
	if t.arrivals%t.snapshotEvery != 0 {
		return
	}
	t.snapshots = append(t.snapshots, t.Snapshot())
}

// insertIndex reports where e belongs in the current canonical order.
func (t *Timeline) insertIndex(e Event) int {
	return sort.Search(len(t.events), func(i int) bool { return Compare(t.events[i], e) > 0 })
}

// cloneEvents copies a slice of events.
func cloneEvents(src []Event) []Event {
	out := make([]Event, len(src))
	copy(out, src)
	return out
}
