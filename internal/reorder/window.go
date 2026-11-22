// Package reorder deduplicates and reorders events within a bounded
// window, and marks unrecoverable gaps without fabricating media.
package reorder

import (
	"mediaroute/internal/adapter"
)

// Window delivers events in sequence order within a bounded buffer.
// Duplicate sequence numbers are counted once and dropped; when the window
// overflows, the missing range is recorded as a gap and delivery continues
// from the newest sequence — no media is fabricated.
type Window struct {
	size    int
	next    uint32
	started bool
	buf     map[uint32]adapter.Event
	seen    map[uint32]struct{} // recently delivered, for duplicate detection

	// Duplicates counts redelivered sequence numbers (each counted once).
	Duplicates int
	// Late counts packets older than the delivered watermark.
	Late int
	// Gaps records ranges skipped on window overflow.
	Gaps []Gap
}

// NewWindow returns a reorder window of the given size (must be > 0).
func NewWindow(size int) *Window {
	if size <= 0 {
		size = 1
	}
	return &Window{
		size: size,
		buf:  make(map[uint32]adapter.Event, size),
		seen: make(map[uint32]struct{}, 2*size),
	}
}

// Push inserts one event and returns any events newly in order.
func (w *Window) Push(ev adapter.Event) (out []adapter.Event) {
	if !w.started {
		w.next = ev.Seq
		w.started = true
	}
	d := int32(ev.Seq - w.next)
	if d < 0 {
		if _, ok := w.seen[ev.Seq]; ok {
			w.Duplicates++
		} else {
			w.Late++
		}
		return nil
	}
	if _, ok := w.buf[ev.Seq]; ok {
		w.Duplicates++
		return nil
	}
	if d >= int32(w.size) {
		out = w.flushBefore(ev.Seq)
	}
	w.buf[ev.Seq] = ev
	return append(out, w.drain()...)
}

// flushBefore delivers every buffered event with seq < limit in order,
// recording one gap per contiguous missing range, and advances next.
func (w *Window) flushBefore(limit uint32) (out []adapter.Event) {
	gapStart, gapOpen := uint32(0), false
	for s := w.next; int32(limit-s) > 0; s++ {
		if e, ok := w.buf[s]; ok {
			if gapOpen {
				w.Gaps = append(w.Gaps, Gap{From: gapStart, To: s - 1})
				gapOpen = false
			}
			out = append(out, e)
			delete(w.buf, s)
			w.markSeen(s)
		} else if !gapOpen {
			gapStart, gapOpen = s, true
		}
	}
	if gapOpen {
		w.Gaps = append(w.Gaps, Gap{From: gapStart, To: limit - 1})
	}
	w.next = limit
	return out
}

// drain delivers the consecutive run starting at next.
func (w *Window) drain() (out []adapter.Event) {
	for {
		e, ok := w.buf[w.next]
		if !ok {
			return out
		}
		out = append(out, e)
		delete(w.buf, w.next)
		w.markSeen(w.next)
		w.next++
	}
}

// Flush delivers everything left in the window at end of stream.
func (w *Window) Flush() (out []adapter.Event) {
	if !w.started {
		return nil
	}
	// Find the highest buffered sequence and flush through it.
	var hi uint32
	found := false
	for s := range w.buf {
		if !found || int32(s-hi) > 0 {
			hi, found = s, true
		}
	}
	if !found {
		return nil
	}
	out = w.flushBefore(hi + 1)
	return append(out, w.drain()...)
}

func (w *Window) markSeen(seq uint32) {
	w.seen[seq] = struct{}{}
	for s := range w.seen {
		if int32(w.next-s) > int32(w.size) {
			delete(w.seen, s)
		}
	}
}

// WindowState is the serializable checkpoint form of a Window.
type WindowState struct {
	Next       uint32          `json:"next"`
	Started    bool            `json:"started"`
	Buf        []adapter.Event `json:"buf,omitempty"`
	Seen       []uint32        `json:"seen,omitempty"`
	Duplicates int             `json:"duplicates"`
	Late       int             `json:"late"`
	Gaps       []Gap           `json:"gaps,omitempty"`
}

// Snapshot captures the window for checkpointing.
func (w *Window) Snapshot() WindowState {
	st := WindowState{
		Next: w.next, Started: w.started,
		Duplicates: w.Duplicates, Late: w.Late, Gaps: append([]Gap(nil), w.Gaps...),
	}
	for _, e := range w.buf {
		st.Buf = append(st.Buf, e)
	}
	for s := range w.seen {
		st.Seen = append(st.Seen, s)
	}
	return st
}

// Restore reinstates a window from a checkpoint.
func (w *Window) Restore(st WindowState) {
	w.next, w.started = st.Next, st.Started
	w.Duplicates, w.Late = st.Duplicates, st.Late
	w.Gaps = append([]Gap(nil), st.Gaps...)
	w.buf = make(map[uint32]adapter.Event, len(st.Buf))
	for _, e := range st.Buf {
		w.buf[e.Seq] = e
	}
	w.seen = make(map[uint32]struct{}, len(st.Seen))
	for _, s := range st.Seen {
		w.seen[s] = struct{}{}
	}
}
