package timeline

import (
	"mediapack/internal/clock"
)

// ReplayWindow is one deterministic backtrack: it covers all committed
// events in [FromNS, ToNS] re-sorted by the stable key. CheckpointNS is
// the last boundary durably recorded; replay must not rewind before it
// (doing so would contradict an already-persisted manifest).
type ReplayWindow struct {
	FromNS       int64
	ToNS         int64
	CheckpointNS int64
	Events       []Event
}

// Replay deterministically recomputes the affected window after a late
// event. It refuses to cross the checkpoint boundary: callers must have
// persisted only up to CheckpointNS, so an earlier late event cannot be
// reconciled and is reported as CatLateEvent.
func (e *Engine) Replay(late Event, checkpointNS int64) (ReplayWindow, error) {
	if late.TimeNS < checkpointNS {
		return ReplayWindow{}, clock.NewError(clock.CatLateEvent,
			"late event at %d predates checkpoint at %d", late.TimeNS, checkpointNS)
	}
	from := late.TimeNS - e.window
	if from < checkpointNS {
		from = checkpointNS
	}
	w := ReplayWindow{
		FromNS:       from,
		ToNS:         e.watermark,
		CheckpointNS: checkpointNS,
	}
	for _, ev := range e.committed {
		if ev.TimeNS >= from && ev.TimeNS <= e.watermark {
			w.Events = append(w.Events, ev)
		}
	}
	sortEvents(w.Events)
	return w, nil
}

// ReplayFrom rebuilds every committed event at or after fromNS using
// the stable ordering. It is the full-window variant used when a
// manifest is regenerated from scratch (recovery path).
func (e *Engine) ReplayFrom(fromNS int64) ReplayWindow {
	w := ReplayWindow{FromNS: fromNS, ToNS: e.watermark, CheckpointNS: fromNS}
	for _, ev := range e.committed {
		if ev.TimeNS >= fromNS {
			w.Events = append(w.Events, ev)
		}
	}
	sortEvents(w.Events)
	return w
}

// Flush forces all pending events to commit at the current maximum time.
// Used at end-of-stream so no in-flight events remain above watermark.
func (e *Engine) Flush() []Event {
	if len(e.pending) == 0 {
		return nil
	}
	maxTime := e.pending[0].TimeNS
	for _, ev := range e.pending {
		if ev.TimeNS > maxTime {
			maxTime = ev.TimeNS
		}
	}
	flushed := make([]Event, len(e.pending))
	copy(flushed, e.pending)
	e.pending = nil
	e.watermark = maxTime
	e.committed = append(e.committed, flushed...)
	e.rebuildCommitted()
	sortEvents(flushed)
	return flushed
}

// Reset returns the engine to its initial state without changing
// configuration (used by recovery tests that rebuild from scratch).
func (e *Engine) Reset() {
	e.watermark = -1
	e.lastSeq = 0
	e.committed = nil
	e.pending = nil
	e.lateCount = 0
	e.dupCount = 0
	e.seenOff = make(map[int64]struct{})
}

func sortEvents(evs []Event) {
	for i := 1; i < len(evs); i++ {
		for j := i; j > 0; j-- {
			a, b := StableKey(evs[j-1]), StableKey(evs[j])
			less := false
			for k := 0; k < 3; k++ {
				if a[k] != b[k] {
					less = a[k] < b[k]
					break
				}
			}
			if !less {
				evs[j-1], evs[j] = evs[j], evs[j-1]
			} else {
				break
			}
		}
	}
}
