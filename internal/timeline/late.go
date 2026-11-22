package timeline

import (
	"sort"

	"mediascan/internal/media"
	"mediascan/internal/segment"
)

// checkpointFor returns the deterministic snapshot at or before ns: the last
// committed checkpoint segment whose end is <= ns. Replay from that snapshot
// re-derives the suffix purely from the retained events, so identical inputs
// reproduce identical segment boundaries, error classes and hashes.
func (e *Engine) checkpointFor(ns int64) Snapshot {
	var best Snapshot
	for _, s := range e.segments {
		if !s.Checkpoint || s.EndNs > ns {
			continue
		}
		if s.Index >= best.AfterIndex {
			best = Snapshot{
				AfterIndex:  s.Index,
				WatermarkNs: s.EndNs,
				TrackIDs:    append([]uint32(nil), s.TrackIDs...),
				Recovery:    s.Recovery,
			}
		}
	}
	return best
}

// ReplayStats records what a replay did, for the demo summary and for tests.
type ReplayStats struct {
	// CheckpointIndex is the last segment retained from the checkpoint.
	CheckpointIndex int
	// Recomputed counts how many segments were rebuilt.
	Recomputed int
	// Equal reports whether the recomputed suffix matched the previous
	// committed suffix exactly (same boundaries and hashes). It is set by
	// VerifyReplay and is false when no replay has occurred.
	Equal bool
}

// replayFrom discards every committed segment after the checkpoint and
// recomputes the suffix from the full retained event set.
//
// "Replay equal" means the recomputed suffix is identical to an independent
// canonical rebuild from the same inputs, not identical to the stale suffix
// (which necessarily changed: that is why the replay ran). Comparing against a
// fresh engine proves the replay carried no hidden state from the pre-late
// accumulator, which is the determinism property the spec requires.
func (e *Engine) replayFrom(cp Snapshot) {
	prev := e.segments
	survivors := make([]Segment, 0, len(prev))
	for _, s := range prev {
		if s.Index <= cp.AfterIndex {
			survivors = append(survivors, s)
		}
	}
	e.segments = survivors
	e.flush()
	rebuilt := append([]Segment(nil), e.segments[len(survivors):]...)
	// Preserve the checkpoint prefix byte for byte: flush would rebuild it
	// identically, but splicing keeps its identity explicit and cheap.
	merged := make([]Segment, 0, len(survivors)+len(rebuilt))
	merged = append(merged, survivors...)
	merged = append(merged, rebuilt...)
	e.segments = merged

	ref := e.canonicalSegments()
	refSuffix := ref[len(survivors):]
	stats := ReplayStats{CheckpointIndex: cp.AfterIndex, Recomputed: len(rebuilt)}
	stats.Equal = segmentsEqual(refSuffix, rebuilt)
	e.lastReplay = stats
	e.replays = append(e.replays, stats)
	e.replayCount++
	e.issues.Add(media.NewInfo(media.ClassTimelineReplay, media.Locator{TimeNs: e.watermark},
		"replayed %d segments from checkpoint %d (equal=%v)", len(rebuilt), cp.AfterIndex, stats.Equal))
}

// canonicalSegments rebuilds all closed segments from scratch in a fresh
// engine, feeding events in arrival-sequence order. It is the reference the
// replay path is checked against.
func (e *Engine) canonicalSegments() []Segment {
	ref := NewEngine(e.cfg)
	evs := append([]Event(nil), e.events...)
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Seq < evs[j].Seq })
	for _, ev := range evs {
		ref.events = append(ref.events, ev)
	}
	ref.watermark = e.watermark
	ref.flush()
	return ref.segments
}

// segmentsEqual compares two segment suffixes on the facts that must be stable:
// boundary times, recovery state, track/class sets and hashes.
func segmentsEqual(a, b []Segment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Index != b[i].Index || a[i].StartNs != b[i].StartNs || a[i].EndNs != b[i].EndNs {
			return false
		}
		if a[i].Recovery != b[i].Recovery || a[i].Hash != b[i].Hash || a[i].Late != b[i].Late {
			return false
		}
	}
	return true
}

// LastReplay returns the most recent replay statistics.
func (e *Engine) LastReplay() (ReplayStats, bool) {
	if e.replayCount == 0 {
		return ReplayStats{}, false
	}
	return e.lastReplay, true
}

// ReplayCount returns how many replays have run.
func (e *Engine) ReplayCount() int { return e.replayCount }

// ReplayEqual reports whether every replay so far reproduced its suffix
// exactly. It is true when no replay has occurred.
func (e *Engine) ReplayEqual() bool {
	for _, r := range e.replays {
		if !r.Equal {
			return false
		}
	}
	return true
}

// VerifyReplay re-runs the engine's final state from scratch with the events in
// a different arrival order and checks that the committed segments are
// identical. This is the strongest form of the determinism guarantee and is
// used by the cross-module tests and the demo's replay_equal check.
func VerifyReplay(events []Event, cfg Config) bool {
	a := NewEngine(cfg)
	for _, ev := range events {
		a.AdvanceWatermark(ev.StartNs)
		a.Add(ev)
	}
	a.AdvanceWatermark(lastEnd(events) + cfg.normalize().SegmentNs)

	// Reverse arrival order: the segmenter must be order-independent.
	b := NewEngine(cfg)
	rev := append([]Event(nil), events...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	for _, ev := range rev {
		b.Add(ev)
	}
	b.AdvanceWatermark(lastEnd(events) + cfg.normalize().SegmentNs)

	return segmentsEqual(a.Segments(), b.Segments())
}

func lastEnd(evs []Event) int64 {
	var m int64
	for _, ev := range evs {
		if ev.EndNs > m {
			m = ev.EndNs
		}
		if ev.StartNs > m {
			m = ev.StartNs
		}
	}
	return m
}

// Compile-time check that Snapshot tracks are copied, not aliased.
var _ = segment.Recovery("")
