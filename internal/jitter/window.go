// Package jitter provides a bounded per-SSRC reorder window. Blocks leave in
// strict RTP sequence order; duplicates are collapsed and packets arriving
// outside the window are reported as classified late errors.
package jitter

import (
	"sort"

	"avcheck/internal/clock"
)

// Outcome tells the caller how an offered block was handled.
type Outcome int

const (
	// OutcomeAccepted means the block is now buffered in the window.
	OutcomeAccepted Outcome = iota
	// OutcomeDuplicate means the sequence number was already seen/played.
	OutcomeDuplicate
	// OutcomeLate means the sequence number is behind the window edge.
	OutcomeLate
	// OutcomeOverflow means the block is ahead of the window by >= capacity.
	OutcomeOverflow
)

// Window is one SSRC's bounded reorder buffer.
type Window struct {
	capacity int
	nextSeq  uint16 // next sequence number expected in order
	started  bool
	buf      map[uint16]*clock.Block
	stats    Stats
}

// Stats exposes window counters.
type Stats struct {
	Accepted   int64
	Duplicates int64
	Late       int64
	Released   int64
}

// NewWindow creates a window sized in packets (capacity >= 1).
func NewWindow(capacity int) *Window {
	if capacity < 1 {
		capacity = 1
	}
	return &Window{capacity: capacity, buf: make(map[uint16]*clock.Block)}
}

// SeqDistance returns the signed RFC 3550 distance from a to b in uint16 space.
func SeqDistance(a, b uint16) int {
	return int(int16(b - a))
}

// Push inserts a block. Late packets return a classified clock.KindLate error;
// overflow packets return clock.KindLate as well but with OutcomeOverflow, so
// the recovery stage can slide its timeline forward.
func (w *Window) Push(b *clock.Block) (Outcome, error) {
	if !w.started {
		w.started = true
		w.nextSeq = b.Seq
	}
	d := SeqDistance(w.nextSeq, b.Seq)
	switch {
	case d < 0:
		w.stats.Late++
		return OutcomeLate, clock.NewErrorAt(clock.KindLate, int64(b.StartTick),
			"ssrc=%d seq=%d behind expected %d", b.SSRC, b.Seq, w.nextSeq)
	case d >= w.capacity:
		w.stats.Late++
		return OutcomeOverflow, clock.NewErrorAt(clock.KindLate, int64(b.StartTick),
			"ssrc=%d seq=%d exceeds window capacity %d (expected %d)",
			b.SSRC, b.Seq, w.capacity, w.nextSeq)
	}
	if _, exists := w.buf[b.Seq]; exists {
		w.stats.Duplicates++
		return OutcomeDuplicate, nil
	}
	w.buf[b.Seq] = b
	w.stats.Accepted++
	return OutcomeAccepted, nil
}

// Drain returns every contiguous in-order block currently available, advancing
// the expected sequence counter past them.
func (w *Window) Drain() []*clock.Block {
	var out []*clock.Block
	for {
		b, ok := w.buf[w.nextSeq]
		if !ok {
			return out
		}
		delete(w.buf, w.nextSeq)
		out = append(out, b)
		w.nextSeq++
		w.stats.Released++
	}
}

// Flush slides the expected sequence number to target and returns the sequence
// numbers skipped while doing so (missing gaps for the recovery stage).
// Buffered blocks at or after target remain in the window.
func (w *Window) Flush(target uint16) []uint16 {
	var missing []uint16
	for SeqDistance(w.nextSeq, target) > 0 {
		if _, ok := w.buf[w.nextSeq]; !ok {
			missing = append(missing, w.nextSeq)
		}
		delete(w.buf, w.nextSeq)
		w.nextSeq++
	}
	return missing
}

// NextSeq reports the sequence number currently expected.
func (w *Window) NextSeq() (uint16, bool) { return w.nextSeq, w.started }

// Buffered reports how many blocks currently occupy the window.
func (w *Window) Buffered() int { return len(w.buf) }

// Snapshot returns buffered blocks ordered by sequence distance.
func (w *Window) Snapshot() []*clock.Block {
	seqs := make([]uint16, 0, len(w.buf))
	for s := range w.buf {
		seqs = append(seqs, s)
	}
	sort.Slice(seqs, func(i, j int) bool {
		return SeqDistance(w.nextSeq, seqs[i]) < SeqDistance(w.nextSeq, seqs[j])
	})
	out := make([]*clock.Block, 0, len(seqs))
	for _, s := range seqs {
		out = append(out, w.buf[s])
	}
	return out
}

// Stats returns a snapshot of the window counters.
func (w *Window) Stats() Stats { return w.stats }
