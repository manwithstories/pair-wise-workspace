package timeline

import (
	"fmt"

	"github.com/clearliu/fieldcut/internal/clock"
)

// ReplayOutcome classifies a late event handed to the engine.
type ReplayOutcome int

const (
	// ReplayAccepted means the event fell inside the look-back window
	// and triggered a deterministic recomputation.
	ReplayAccepted ReplayOutcome = iota
	// ReplayIsolated means it arrived outside the window; it is retained
	// in a quarantine report but never mutates committed timeline state.
	ReplayIsolated
)

// ReplayReport is one deterministic record of a late-event decision.
type ReplayReport struct {
	Event   *Event
	Outcome ReplayOutcome
	// RecomputedFrom is the Start tick from which the timeline was
	// rebuilt when the event was accepted.
	RecomputedFrom clock.Ticks
}

// Checkpoint is a resumable position. It contains no packet payloads, so
// buffers can be released on cancellation/failure and restarted from the
// last checkpoint without re-reading quarantined data.
type Checkpoint struct {
	CommittedThrough clock.Ticks
	EventCount       int
	Window           clock.Ticks
}

// ReplayEngine keeps the committed timeline and decides what a late event
// is allowed to change. Recomputation always re-sorts the affected tail
// from the same input, so reruns produce identical output.
type ReplayEngine struct {
	window clock.Ticks
	// committed is the immutable prefix in total order.
	committed []*Event
	// quarantine holds events rejected for being too late.
	quarantine []ReplayReport
	replays    int
	isolated   int
}

// NewReplayEngine builds an engine. window <= 0 selects a 4s default and
// every value is hard-capped by clock.MaxLateWindowTicks.
func NewReplayEngine(window clock.Ticks) *ReplayEngine {
	if window <= 0 {
		window = clock.Ticks(4 * clock.Rate)
	}
	if window > clock.Ticks(clock.MaxLateWindowTicks) {
		window = clock.Ticks(clock.MaxLateWindowTicks)
	}
	return &ReplayEngine{window: window}
}

// Append adds an already watermark-released event to the committed
// prefix, keeping total order.
func (e *ReplayEngine) Append(ev *Event) {
	e.committed = append(e.committed, ev)
}

// IngestLate handles one late event against the current timeline head.
// Accepted events are spliced in and the tail is recomputed
// deterministically; isolated events are reported but never spliced.
func (e *ReplayEngine) IngestLate(ev *Event) ReplayReport {
	head := e.head()
	rep := ReplayReport{Event: ev}
	if head-ev.Start > e.window || ev.Start > head {
		// Outside the look-back window (or implausibly far in the
		// future): quarantine, leave committed state untouched.
		rep.Outcome = ReplayIsolated
		e.quarantine = append(e.quarantine, rep)
		e.isolated++
		return rep
	}
	rep.Outcome = ReplayAccepted
	rep.RecomputedFrom = ev.Start
	// Splice then re-sort; sortEvents is a stable total order so the
	// result depends only on the event set, never on arrival timing.
	e.committed = append(e.committed, ev)
	sortEvents(e.committed)
	e.replays++
	return rep
}

// Recompute forces a deterministic rebuild of every event at or after
// from. It exists so callers (e.g. an RTX fill) can trigger the same
// recomputation path without inventing a late event.
func (e *ReplayEngine) Recompute(from clock.Ticks) []*Event {
	var tail []*Event
	for _, ev := range e.committed {
		if ev.Start >= from {
			tail = append(tail, ev)
		}
	}
	sortEvents(tail)
	return tail
}

func (e *ReplayEngine) head() clock.Ticks {
	if len(e.committed) == 0 {
		return 0
	}
	return e.committed[len(e.committed)-1].Start
}

// Committed returns a copy of the committed timeline.
func (e *ReplayEngine) Committed() []*Event {
	out := make([]*Event, len(e.committed))
	copy(out, e.committed)
	return out
}

// Quarantine returns the isolation reports (no mutation).
func (e *ReplayEngine) Quarantine() []ReplayReport {
	out := make([]ReplayReport, len(e.quarantine))
	copy(out, e.quarantine)
	return out
}

// Stats returns deterministic replay counters.
func (e *ReplayEngine) Stats() (replays, isolated int) {
	return e.replays, e.isolated
}

// Checkpoint captures the current resumable position.
func (e *ReplayEngine) Checkpoint() Checkpoint {
	return Checkpoint{
		CommittedThrough: e.head(),
		EventCount:       len(e.committed),
		Window:           e.window,
	}
}

// Resume verifies an external checkpoint matches engine state. A
// mismatch returns clock.ErrCheckpoint rather than silently rebuilding a
// different timeline.
func (e *ReplayEngine) Resume(cp Checkpoint) error {
	if cp.Window != e.window {
		return fmt.Errorf("%w: window %d != %d", clock.ErrCheckpoint, cp.Window, e.window)
	}
	if cp.EventCount > len(e.committed) {
		return fmt.Errorf("%w: event count %d > committed %d",
			clock.ErrCheckpoint, cp.EventCount, len(e.committed))
	}
	if cp.EventCount == len(e.committed) && cp.CommittedThrough != e.head() {
		return fmt.Errorf("%w: head %d != %d",
			clock.ErrCheckpoint, cp.CommittedThrough, e.head())
	}
	return nil
}
