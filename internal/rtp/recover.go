// Package rtp recovers RTP packet sequences across wrap-around, duplicates
// and reordering. Packets inside the configured reorder window are buffered
// and emitted in order; sequence numbers that the window passes over are
// counted as lost, and duplicates never count as recovery.
package rtp

import (
	"github.com/pion/rtp"

	merr "mediaroute/internal/errors"
)

// DefaultReorderWindow is the number of sequence slots the buffer holds.
const DefaultReorderWindow = 64

// maxSpanFactor bounds how far ahead of the base a packet may sit before the
// base is forced forward. The effective horizon is the larger of the
// configured window and two sequence wraps' worth of headroom; this keeps the
// window a reorder bound rather than an unbounded hold time.
const maxSpanFactor = 2

// Stats summarises one recovery run.
type Stats struct {
	// Received counts every packet handed to the buffer, including duplicates.
	Received int
	// Delivered counts unique packets emitted in order.
	Delivered int
	// Recovered counts packets that arrived out of order but inside the
	// reorder window and were delivered, rather than counted as lost.
	Recovered int
	// Lost counts sequence slots the window passed over without a packet.
	Lost int
	// Duplicates counts re-arrivals of an already buffered or emitted packet.
	Duplicates int
	// LateDropped counts packets whose slot was already passed and could not
	// be delivered.
	LateDropped int
	// ReorderDropped counts packets that arrived out of order but could not be
	// recovered because the reorder window had already passed their slot.
	ReorderDropped int
	// Wraps counts 16-bit sequence wrap-arounds observed.
	Wraps int
	// MaxReorderDistance is the largest backward jump seen, in sequence
	// numbers.
	MaxReorderDistance int64
}

// RecoveryRate is the fraction of out-of-order packets that were successfully
// recovered inside the reorder window: Recovered/(Recovered+ReorderDropped).
// Unrecovered gaps are reported separately by Lost and never counted here, and
// duplicates never enter either numerator or denominator.
func (s Stats) RecoveryRate() float64 {
	den := s.Recovered + s.ReorderDropped
	if den == 0 {
		return 1
	}
	return float64(s.Recovered) / float64(den)
}

// LossRatio is Lost/(Delivered+Lost); it quantifies genuine stream loss such
// as a burst that the network dropped entirely.
func (s Stats) LossRatio() float64 {
	den := s.Delivered + s.Lost
	if den == 0 {
		return 0
	}
	return float64(s.Lost) / float64(den)
}

// Recoverer reorders a packet stream using extended sequence numbers.
type Recoverer struct {
	Window int

	initialized bool
	base        int64 // extended sequence of the next expected packet
	highest     int64 // highest extended sequence seen
	lastExt     int64 // previous arrival, for reorder distance
	pending     map[int64]*rtp.Packet
	emitted     map[int64]bool // recent deliveries for duplicate detection
	stats       Stats
}

// New creates a Recoverer with the given reorder window (default if <= 0).
func New(window int) *Recoverer {
	if window <= 0 {
		window = DefaultReorderWindow
	}
	return &Recoverer{
		Window:  window,
		pending: map[int64]*rtp.Packet{},
		emitted: map[int64]bool{},
	}
}

// extend maps a 16-bit sequence number onto the extended space referenced to
// the highest value seen, so wrap-around is a normal increment.
func extend(seq uint16, highest int64) int64 {
	cand := (highest &^ 0xffff) | int64(seq)
	switch {
	case cand-highest > 1<<15:
		cand -= 1 << 16
	case highest-cand > 1<<15:
		cand += 1 << 16
	}
	return cand
}

// Push feeds one packet and returns every packet that is now in order.
func (r *Recoverer) Push(pkt *rtp.Packet) ([]*rtp.Packet, error) {
	if pkt == nil {
		return nil, merr.New(merr.KindRTP, 0, -1, -1, false, "nil RTP packet")
	}
	r.stats.Received++
	seq := pkt.SequenceNumber

	if !r.initialized {
		r.initialized = true
		r.base = int64(seq)
		r.highest = int64(seq)
		r.lastExt = int64(seq)
		r.pending[r.base] = pkt
		return r.drain(), nil
	}

	ext := extend(seq, r.highest)
	if (ext&^0xffff) != (r.highest&^0xffff) && ext > r.highest {
		r.stats.Wraps++
	}
	if ext > r.highest {
		r.highest = ext
	}
	if ext < r.base {
		// The prefix was already delivered. A re-arrival is a duplicate; a new
		// packet this late is dropped and its slot stays counted as lost (or is
		// declared lost on flush), never counted as recovery.
		if r.emitted[ext] {
			r.stats.Duplicates++
			return nil, nil
		}
		// A brand-new packet whose slot was already passed: if it arrived out
		// of order relative to its neighbours it was a reorder the window could
		// not absorb; otherwise it is simply late.
		if r.highest-ext > int64(r.Window) {
			r.stats.ReorderDropped++
		} else {
			r.stats.LateDropped++
		}
		return nil, nil
	}
	if _, seen := r.pending[ext]; seen {
		r.stats.Duplicates++
		return nil, nil
	}
	if r.emitted[ext] {
		r.stats.Duplicates++
		return nil, nil
	}
	if ext != r.lastExt+1 {
		if dist := r.lastExt - ext; dist > 0 {
			if dist > r.stats.MaxReorderDistance {
				r.stats.MaxReorderDistance = dist
			}
			r.stats.Recovered++
		}
	}
	r.lastExt = ext
	r.pending[ext] = pkt

	// Bound memory: if arrivals run further ahead than two windows, advance the
	// base and declare the skipped slots lost.
	if r.highest-r.base >= int64(maxSpanFactor*r.Window) {
		for r.highest-r.base >= int64(r.Window) {
			if _, ok := r.pending[r.base]; ok {
				break
			}
			r.stats.Lost++
			r.base++
		}
	}
	return r.drain(), nil
}

// drain emits in-order packets starting at base and stops at the first gap
// while the gap is still inside the window.
func (r *Recoverer) drain() []*rtp.Packet {
	var out []*rtp.Packet
	for {
		if pkt, ok := r.pending[r.base]; ok {
			out = append(out, pkt)
			delete(r.pending, r.base)
			r.emitted[r.base] = true
			r.pruneEmitted()
			r.base++
			r.stats.Delivered++
			continue
		}
		if r.highest-r.base >= int64(r.Window) {
			r.stats.Lost++
			r.base++
			continue
		}
		break
	}
	return out
}

// pruneEmitted keeps the duplicate-detection window bounded.
func (r *Recoverer) pruneEmitted() {
	if len(r.emitted) < 4*r.Window {
		return
	}
	for k := range r.emitted {
		if k < r.base-int64(r.Window) {
			delete(r.emitted, k)
		}
	}
}

// Flush declares every remaining gap lost and emits the rest in order.
func (r *Recoverer) Flush() []*rtp.Packet {
	var out []*rtp.Packet
	if !r.initialized {
		return out
	}
	for r.base <= r.highest {
		if pkt, ok := r.pending[r.base]; ok {
			out = append(out, pkt)
			r.stats.Delivered++
		} else {
			r.stats.Lost++
		}
		r.base++
	}
	r.pending = map[int64]*rtp.Packet{}
	return out
}

// Stats returns a snapshot of recovery counters.
func (r *Recoverer) Stats() Stats { return r.stats }

// Buffered returns the number of packets currently held out of order.
func (r *Recoverer) Buffered() int { return len(r.pending) }
