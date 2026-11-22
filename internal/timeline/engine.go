// Package timeline maintains a watermark-driven event time line on the
// shared nanosecond clock. Events only become committed once they cross
// the watermark; events arriving after the watermark are "late" and
// trigger a deterministic, checkpoint-bounded replay of the affected
// window. Repeated input always produces the same committed sequence.
package timeline

import (
	"sort"

	"mediapack/internal/clock"
)

// Event is one time-ordered unit derived from an EBML block.
type Event struct {
	// Seq is the monotonic arrival order assigned by the parser feed.
	Seq int64
	// Track is the EBML track number the block belongs to.
	Track uint64
	// TimeNS is the cluster timecode plus block relative timecode,
	// converted to the shared nanosecond base (wrap already resolved).
	TimeNS int64
	// RawTime is the pre-conversion tick value, retained for diagnostics.
	RawTime int64
	// ByteOff is the block element's absolute offset in the input.
	ByteOff int64
	// PayloadLen is the block payload size in bytes.
	PayloadLen int64
	// Keyframe mirrors the SimpleBlock keyframe flag.
	Keyframe bool
}

// StableKey defines the total ordering used for replay and manifests:
// time first, then arrival sequence, then track — so equal timestamps
// never depend on map iteration or goroutine scheduling.
func StableKey(e Event) [3]int64 {
	return [3]int64{e.TimeNS, e.Seq, int64(e.Track)}
}

// Engine is the watermark state machine.
type Engine struct {
	tb         *clock.Timebase
	watermark  int64 // highest committed time in nanos
	leadNS     int64 // event must be <= maxTime-lead to be committable
	window     int64 // replay retention window in nanos
	lastSeq    int64
	committed  []Event
	pending    []Event
	lateCount  int
	dupCount   int
	seenOff    map[int64]struct{} // byte offsets already accepted
}

// Config tunes an Engine. LeadNS is the watermark lag; WindowNS bounds
// how far back a late event can reach (older events are rejected).
type Config struct {
	Timebase *clock.Timebase
	LeadNS   int64
	WindowNS int64
}

// NewEngine constructs an engine. Zero lead means events commit only
// when a strictly later event advances the maximum observed time.
func NewEngine(cfg Config) *Engine {
	tb := cfg.Timebase
	if tb == nil {
		tb = clock.DefaultTimebase()
	}
	window := cfg.WindowNS
	if window <= 0 {
		window = 10_000_000_000 // retain 10s by default
	}
	return &Engine{
		tb:      tb,
		leadNS:  cfg.LeadNS,
		window:  window,
		// -1 means "no watermark established yet": the first event can
		// never satisfy TimeNS <= watermark and therefore stays pending.
		watermark: -1,
		seenOff: make(map[int64]struct{}),
	}
}

// PushResult reports what happened to one pushed event.
type PushResult struct {
	Event      Event
	Committed  []Event // newly committed events from this push
	Late       bool
	Duplicate  bool
	Rejected   bool
	Reason     clock.Category // CatLateEvent when outside replay window
}

// Push feeds one event. The arrival sequence is assigned here when the
// caller leaves Seq zero, preserving parser order for the stable key.
func (e *Engine) Push(ev Event) PushResult {
	if ev.Seq == 0 {
		e.lastSeq++
		ev.Seq = e.lastSeq
	} else if ev.Seq > e.lastSeq {
		e.lastSeq = ev.Seq
	}
	res := PushResult{Event: ev}

	// Byte-offset identity: the same physical block fed twice is a dup.
	if _, ok := e.seenOff[ev.ByteOff]; ok {
		e.dupCount++
		res.Duplicate = true
		return res
	}

	// A time strictly below the established watermark is late; an event
	// exactly at the watermark is still in-order (boundary inclusive).
	if ev.TimeNS < e.watermark {
		return e.pushLate(ev, &res)
	}

	e.seenOff[ev.ByteOff] = struct{}{}
	e.pending = append(e.pending, ev)
	res.Committed = e.advanceWatermark()
	return res
}

// pushLate handles an event whose time is not above the watermark.
func (e *Engine) pushLate(ev Event, res *PushResult) PushResult {
	if e.watermark-ev.TimeNS > e.window {
		res.Rejected = true
		res.Reason = clock.CatLateEvent
		return *res
	}
	if _, ok := e.seenOff[ev.ByteOff]; ok {
		e.dupCount++
		res.Duplicate = true
		return *res
	}
	e.seenOff[ev.ByteOff] = struct{}{}
	e.lateCount++
	res.Late = true
	// Reinsert into the committed sequence; Replay rebuilds order.
	e.committed = append(e.committed, ev)
	e.rebuildCommitted()
	// The late event becomes part of the committed set for replay.
	res.Committed = []Event{ev}
	return *res
}

// advanceWatermark recomputes the watermark as max(pending)-lead and
// commits pending events strictly below the *new* watermark. The event
// establishing the maximum stays pending, so nothing commits on the
// very first push: an event commits only once a later arrival moves the
// watermark past it ("events only commit once they cross watermark").
func (e *Engine) advanceWatermark() []Event {
	if len(e.pending) == 0 {
		return nil
	}
	maxTime := e.pending[0].TimeNS
	for _, ev := range e.pending {
		if ev.TimeNS > maxTime {
			maxTime = ev.TimeNS
		}
	}
	newWM := maxTime - e.leadNS
	if newWM <= e.watermark {
		return nil // watermark cannot move backwards
	}
	var newly, remaining []Event
	for _, ev := range e.pending {
		// Commit events the advancing watermark has strictly passed;
		// the maximum event (at/above newWM) stays pending.
		if ev.TimeNS < newWM {
			newly = append(newly, ev)
		} else {
			remaining = append(remaining, ev)
		}
	}
	e.pending = remaining
	e.watermark = newWM
	e.committed = append(e.committed, newly...)
	e.rebuildCommitted()
	return newly
}

// rebuildCommitted sorts the committed slice by the stable total key,
// giving deterministic output regardless of arrival/late insertion.
func (e *Engine) rebuildCommitted() {
	sort.SliceStable(e.committed, func(i, j int) bool {
		a, b := StableKey(e.committed[i]), StableKey(e.committed[j])
		for k := 0; k < 3; k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return false
	})
}

// Watermark returns the current committed watermark in nanoseconds.
func (e *Engine) Watermark() int64 { return e.watermark }

// Committed returns a copy of the committed events in stable order.
func (e *Engine) Committed() []Event {
	out := make([]Event, len(e.committed))
	copy(out, e.committed)
	return out
}

// Pending returns a copy of events still above the watermark.
func (e *Engine) Pending() []Event {
	out := make([]Event, len(e.pending))
	copy(out, e.pending)
	return out
}

// LateCount reports how many accepted events arrived post-watermark.
func (e *Engine) LateCount() int { return e.lateCount }

// DupCount reports how many pushes were rejected as duplicates.
func (e *Engine) DupCount() int { return e.dupCount }

// CommittedCount returns the number of committed events.
func (e *Engine) CommittedCount() int { return len(e.committed) }
