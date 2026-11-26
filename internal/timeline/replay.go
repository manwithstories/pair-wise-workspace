package timeline

import (
	"mediad/internal/media"
)

// Snapshot is a prefix of the accepted event list plus the watermark at the
// moment it was taken. Snapshots are taken every few arrivals so a replay can
// restart from a nearby prefix instead of rescanning from the beginning.
type Snapshot struct {
	Len       int             `json:"len"`
	Watermark media.MediaTick `json:"watermark"`
	Events    []Event         `json:"events"`
}

// Snapshot returns a deep copy of the current state for checkpointing.
func (t *Timeline) Snapshot() Snapshot {
	return Snapshot{Len: len(t.events), Watermark: t.watermark, Events: cloneEvents(t.events)}
}

// Restore replaces the whole timeline state from a snapshot and rebuilds the
// snapshot ladder around it. Statistics (late/replayed counters) are restored
// by the checkpoint layer separately.
func (t *Timeline) Restore(s Snapshot) {
	t.started = len(s.Events) > 0
	t.watermark = s.Watermark
	t.events = cloneEvents(s.Events)
	t.snapshots = []Snapshot{t.Snapshot()}
	t.arrivals = len(t.events)
}

// replay rolls back to the nearest snapshot at or before the insertion point,
// inserts the late event and re-applies the detached suffix through the same
// deterministic insertion routine. The result equals a clean sequential run.
func (t *Timeline) replay(e Event) int {
	target := t.insertIndex(e)
	base := t.snapshotAtOrBefore(target)
	if base == nil {
		base = &Snapshot{}
	}

	// Detach the suffix that the snapshot does not cover.
	suffix := cloneEvents(t.events[base.Len:])

	// Restore the snapshot prefix.
	t.events = cloneEvents(base.Events)
	// Keep the watermark monotonic: a replay never rewinds event time.
	if base.Watermark > t.watermark {
		t.watermark = base.Watermark
	}

	// Record the recomputed span for output invalidation.
	t.lastReplayFrom = e.Time
	t.lastReplayTo = e.Time
	if n := len(suffix); n > 0 {
		if suffix[0].Time < t.lastReplayFrom {
			t.lastReplayFrom = suffix[0].Time
		}
		if suffix[n-1].Time > t.lastReplayTo {
			t.lastReplayTo = suffix[n-1].Time
		}
	}

	// Re-apply the late event and the suffix in canonical order.
	t.insert(e)
	for _, v := range suffix {
		t.insert(v)
	}
	if e.Time > t.watermark {
		t.watermark = e.Time
	}
	if n := len(suffix); n > 0 && suffix[n-1].Time > t.watermark {
		t.watermark = suffix[n-1].Time
	}

	// Drop snapshots that the replay invalidated, then take one at the new tip.
	kept := t.snapshots[:0]
	for _, s := range t.snapshots {
		if s.Len <= len(t.events) {
			kept = append(kept, s)
		}
	}
	t.snapshots = kept
	t.maybeSnapshot()

	return len(suffix)
}

// snapshotAtOrBefore returns the newest snapshot whose prefix length is at
// most n.
func (t *Timeline) snapshotAtOrBefore(n int) *Snapshot {
	best := -1
	for i := range t.snapshots {
		if t.snapshots[i].Len <= n && t.snapshots[i].Len >= best {
			best = t.snapshots[i].Len
		}
	}
	if best < 0 {
		return nil
	}
	for i := len(t.snapshots) - 1; i >= 0; i-- {
		if t.snapshots[i].Len == best {
			return &t.snapshots[i]
		}
	}
	return nil
}
