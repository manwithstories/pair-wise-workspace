// Package rtp implements a bounded RTP receive window: duplicate
// sequence numbers are counted once, in-window reordering is delivered
// in order, window overflow is recorded as a gap without fabricating
// media, and late packets outside the window never rewrite a
// already-committed timeline.
package rtp

import (
	"errors"
	"fmt"
	"sync"

	"github.com/pion/rtp"
)

// ErrWindowOverflow reports a jump larger than the configured window.
var ErrWindowOverflow = errors.New("rtp: sequence jump exceeds window capacity")

// Metrics summarises window behaviour for the run summary.
type Metrics struct {
	Received   int // unique in-window packets accepted
	Duplicates int // repeated sequence numbers, counted once
	Reordered  int // packets that filled an earlier gap
	LateDrop   int // packets arriving past the committed frontier
	Gaps       int // distinct gap events
	GapPackets int // total missing sequence numbers observed
	Recovered  int // missing sequences later filled before flush
}

// RecoveryRate returns filled gaps / total gap packets; 1.0 if none.
func (m Metrics) RecoveryRate() float64 {
	if m.GapPackets == 0 {
		return 1
	}
	return float64(m.Recovered) / float64(m.GapPackets)
}

// Window is a fixed-capacity reorder buffer keyed by 16-bit RTP sequence.
type Window struct {
	mu sync.Mutex

	capacity  int
	slots     map[uint16]*rtp.Packet
	missing   map[uint16]struct{} // sequences of opened, still-open gaps
	delivered map[uint16]struct{} // sequences already handed to the consumer
	nextOut   uint16              // next sequence number expected for delivery
	high      uint16              // highest sequence ever accepted
	haveHigh  bool
	started   bool
	committed uint16 // one past the last flushed sequence
	haveFlush bool

	m Metrics
}

// NewWindow creates a window holding at most capacity out-of-order packets.
func NewWindow(capacity int) (*Window, error) {
	if capacity <= 0 || capacity > 1<<15 {
		return nil, fmt.Errorf("rtp: window capacity out of range: %d", capacity)
	}
	return &Window{
		capacity:  capacity,
		slots:     map[uint16]*rtp.Packet{},
		missing:   map[uint16]struct{}{},
		delivered: map[uint16]struct{}{},
	}, nil
}

// Push accepts a packet. Returned packets are in strict sequence order;
// a nil slice means nothing became deliverable yet. Overflow and late
// arrivals are accounted for but never produce synthetic packets.
func (w *Window) Push(pkt *rtp.Packet) ([]*rtp.Packet, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	seq := pkt.SequenceNumber

	if w.haveFlush && !seqGE(seq, w.committed) {
		// Outside-window late packet: the timeline is already final
		// for this sequence, so it must not alter it.
		w.m.LateDrop++
		return nil, nil
	}
	if !w.started {
		w.started = true
		w.nextOut = seq
		w.high = seq
		w.haveHigh = true
	}
	if _, exists := w.slots[seq]; exists {
		w.m.Duplicates++
		return nil, nil
	}
	// Already handed to the consumer: re-arrival is a duplicate, not a
	// reorder fill, and must not alter the committed timeline.
	if _, done := w.delivered[seq]; done {
		w.m.Duplicates++
		return nil, nil
	}

	// Only sequences beyond the previous high-water mark open new gap
	// accounting. Packets inside [nextOut, high) are reorder arrivals
	// that fill an already-counted hole, never a second gap.
	forward := seqForward(w.high, seq)
	if w.haveHigh && forward > 0 {
		newMissing := forward - 1 // steps strictly between high and seq
		if forward > w.capacity {
			// Hole too large to buffer: count it, drop stale buffer,
			// anchor at the new packet. No media is invented.
			w.m.Gaps++
			w.m.GapPackets += forward
			w.nextOut = seq
			w.slots = map[uint16]*rtp.Packet{}
			w.missing = map[uint16]struct{}{}
			w.delivered = map[uint16]struct{}{}
		} else if newMissing > 0 {
			w.m.Gaps++
			w.m.GapPackets += newMissing
			for i := 1; i <= newMissing; i++ {
				w.missing[w.high+uint16(i)] = struct{}{}
			}
		}
		w.high = seq
	}

	if len(w.slots) >= w.capacity {
		// Hard capacity guard: refuse to grow beyond the budget.
		return nil, ErrWindowOverflow
	}
	w.slots[seq] = pkt
	w.m.Received++
	return w.drainLocked(), nil
}

// Flush forces delivery of everything currently contiguous, records
// still-missing sequences as unrecovered gaps, and advances the
// committed frontier so later late packets are rejected.
func (w *Window) Flush() []*rtp.Packet {
	w.mu.Lock()
	defer w.mu.Unlock()

	out := w.drainLocked()
	// Gaps were already counted when opened in Push; any buffered
	// packets left behind are discarded as unrecovered tail content.
	// Advance the frontier past the furthest buffered sequence so
	// subsequent late packets cannot resurrect committed timeline.
	if len(w.slots) > 0 {
		var maxSeq uint16
		first := true
		for s := range w.slots {
			if first || seqForward(w.nextOut, s) > seqForward(w.nextOut, maxSeq) {
				maxSeq, first = s, false
			}
		}
		w.slots = map[uint16]*rtp.Packet{}
		w.missing = map[uint16]struct{}{}
		w.nextOut = maxSeq + 1
	}
	w.committed = w.nextOut
	w.haveFlush = true
	return out
}

// drainLocked emits the contiguous in-order prefix. A packet whose
// sequence was part of an opened gap counts as reorder recovery.
func (w *Window) drainLocked() []*rtp.Packet {
	out := []*rtp.Packet{}
	for {
		pkt, ok := w.slots[w.nextOut]
		if !ok {
			break
		}
		if _, wasMissing := w.missing[w.nextOut]; wasMissing {
			delete(w.missing, w.nextOut)
			w.m.Recovered++
			w.m.Reordered++
		}
		out = append(out, pkt)
		delete(w.slots, w.nextOut)
		w.delivered[w.nextOut] = struct{}{}
		w.nextOut++
	}
	return out
}

// Metrics returns a snapshot of window counters.
func (w *Window) Metrics() Metrics {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.m
}

// seqGE compares 16-bit sequence numbers with modular arithmetic.
func seqGE(a, b uint16) bool { return int16(a-b) >= 0 }

// seqForward returns the signed forward distance from a to b modulo 2^16.
func seqForward(a, b uint16) int { return int(int16(b - a)) }
