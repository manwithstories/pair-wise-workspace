// Package jitter implements the bounded, event-time ordered playout window.
// Packets arrive in network order but must leave in media-time order; the
// window performs that reordering while keeping memory strictly bounded and
// evicting expired events with a classified reason instead of growing forever.
package jitter

import (
	"sort"

	"mediakernel/internal/model"
)

// Config bounds window resources.
type Config struct {
	// MaxEvents is the hard cap on buffered events across all streams.
	MaxEvents int
	// MaxLateTolerance lets a caller flag extreme reordering; eviction
	// decisions themselves only depend on capacity.
	MaxLateToleranceTicks int64
}

// DefaultConfig returns safe simulation-scale bounds.
func DefaultConfig() Config {
	return Config{MaxEvents: 2048, MaxLateToleranceTicks: 2_000_000}
}

// Window is a bounded buffer ordered by (EventTime, Stream, Seq).
type Window struct {
	cfg       Config
	events    []*model.Event
	log       *model.Log
	reordered int
	evicted   int
	maxTime   int64
	started   bool
}

// New creates an empty window backed by the given fault log.
func New(cfg Config, log *model.Log) *Window {
	if cfg.MaxEvents <= 0 {
		cfg = DefaultConfig()
	}
	return &Window{cfg: cfg, log: log}
}

// Push inserts one accepted event in event-time order. An event whose media
// time precedes the highest seen so far counts as a reorder. When the buffer
// is at capacity the oldest event is evicted as expired and the decision is
// recorded, so memory can never grow past MaxEvents.
func (w *Window) Push(ev *model.Event) {
	if w.started && int64(ev.EventTime) < w.maxTime {
		w.reordered++
	}
	if !w.started || int64(ev.EventTime) > w.maxTime {
		w.maxTime = int64(ev.EventTime)
		w.started = true
	}
	if len(w.events) >= w.cfg.MaxEvents {
		w.evictOldest()
	}
	idx := sort.Search(len(w.events), func(i int) bool {
		return less(ev, w.events[i])
	})
	w.events = append(w.events, nil)
	copy(w.events[idx+1:], w.events[idx:])
	w.events[idx] = ev
}

func less(a, b *model.Event) bool {
	if a.EventTime != b.EventTime {
		return a.EventTime < b.EventTime
	}
	if a.Stream != b.Stream {
		return a.Stream < b.Stream
	}
	return a.Seq < b.Seq
}

// evictOldest removes the head of the window and records why. The incoming
// event is passed to anchor the record on the current media time.
func (w *Window) evictOldest() {
	victim := w.events[0]
	w.events = w.events[1:]
	w.evicted++
	w.log.Add(model.MediaError{
		Category: model.CatOverflow,
		Stream:   victim.Stream,
		Seq:      victim.Seq,
		At:       victim.EventTime,
		Detail:   "jitter window full; dropping oldest expired event",
		Decision: model.DecDropped,
	})
}

// DrainUpTo removes and returns every event with EventTime <= cutoff in
// deterministic event-time order. The remaining window stays sorted.
func (w *Window) DrainUpTo(cutoff int64) []*model.Event {
	idx := sort.Search(len(w.events), func(i int) bool {
		return int64(w.events[i].EventTime) > cutoff
	})
	out := make([]*model.Event, idx)
	copy(out, w.events[:idx])
	w.events = w.events[idx:]
	return out
}

// Peek returns the oldest buffered event without removing it, or nil.
func (w *Window) Peek() *model.Event {
	if len(w.events) == 0 {
		return nil
	}
	return w.events[0]
}

// Len is the current buffered event count.
func (w *Window) Len() int { return len(w.events) }

// Reordered returns how many pushes arrived out of event-time order.
func (w *Window) Reordered() int { return w.reordered }

// Evicted returns how many events were dropped due to capacity pressure.
func (w *Window) Evicted() int { return w.evicted }

// Snapshot returns the buffered events in order without mutating the window.
func (w *Window) Snapshot() []*model.Event {
	out := make([]*model.Event, len(w.events))
	copy(out, w.events)
	return out
}
