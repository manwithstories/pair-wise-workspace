// Package rtp reorders and deduplicates RTP packets inside a bounded
// window and classifies every gap as recovered, unrecoverable, or late.
// Retried packets that were already accounted for are never counted twice.
package rtp

import (
	"github.com/pion/rtp"
)

// MaxWindow is the hard cap on the reorder window.
const MaxWindow = 512

const (
	accountedEmitted = 0
	accountedLost    = 1
)

// Stats summarizes one recovered stream.
type Stats struct {
	Received   int     `json:"received"`    // unique packets emitted, in-order arrivals
	Recovered  int     `json:"recovered"`   // filled a gap before it expired
	Lost       int     `json:"lost"`        // gap expired, unrecoverable
	Late       int     `json:"late"`        // arrived after being declared lost
	Duplicates int     `json:"duplicates"`  // already accounted; never re-counted
	Runs       int     `json:"runs"`        // contiguous usable intervals emitted
	RecoveryPct float64 `json:"recovery_pct"`
}

type pendingPkt struct {
	pkt        rtp.Packet
	outOfOrder bool
}

// Recoverer is a per-stream reorder/dedup state machine.
type Recoverer struct {
	window    int
	started   bool
	next      uint16
	pending   map[uint16]pendingPkt
	accounted map[uint16]uint8 // seq -> emitted|lost, bounded by 2^16
	stats     Stats
	inRun     bool
}

// NewRecoverer creates a Recoverer with the given window, clamped to
// [1, MaxWindow].
func NewRecoverer(window int) *Recoverer {
	if window < 1 {
		window = 1
	}
	if window > MaxWindow {
		window = MaxWindow
	}
	return &Recoverer{
		window:    window,
		pending:   make(map[uint16]pendingPkt),
		accounted: make(map[uint16]uint8),
	}
}

// Feed consumes one packet in arrival order.
func (r *Recoverer) Feed(p rtp.Packet) {
	seq := p.SequenceNumber
	if !r.started {
		r.next = seq
		r.started = true
	}
	if st, seen := r.accounted[seq]; seen {
		if st == accountedLost {
			r.stats.Late++
		} else {
			r.stats.Duplicates++
		}
		return
	}
	if _, dup := r.pending[seq]; dup {
		r.stats.Duplicates++
		return
	}
	r.pending[seq] = pendingPkt{pkt: p, outOfOrder: seq != r.next}
	r.drain()
	for len(r.pending) > r.window {
		r.expire()
	}
}

// Flush terminates the stream: pending packets are emitted in order and
// every remaining gap is declared lost.
func (r *Recoverer) Flush() {
	for len(r.pending) > 0 {
		if _, ok := r.pending[r.next]; ok {
			r.drain()
		} else {
			r.expire()
		}
	}
	r.endRun()
}

// Stats returns the final counts, computing the recovery percentage.
func (r *Recoverer) Stats() Stats {
	s := r.stats
	gaps := s.Recovered + s.Lost
	if gaps == 0 {
		s.RecoveryPct = 100
	} else {
		s.RecoveryPct = float64(s.Recovered) / float64(gaps) * 100
	}
	return s
}

// drain emits every contiguous packet starting at r.next.
func (r *Recoverer) drain() {
	for {
		pp, ok := r.pending[r.next]
		if !ok {
			return
		}
		delete(r.pending, r.next)
		r.accounted[r.next] = accountedEmitted
		if pp.outOfOrder {
			r.stats.Recovered++
		} else {
			r.stats.Received++
		}
		r.beginRun()
		r.next++
	}
}

// expire declares the packet at r.next unrecoverable and moves on.
func (r *Recoverer) expire() {
	r.accounted[r.next] = accountedLost
	r.stats.Lost++
	r.endRun()
	r.next++
	r.drain()
}

func (r *Recoverer) beginRun() {
	if !r.inRun {
		r.inRun = true
		r.stats.Runs++
	}
}

func (r *Recoverer) endRun() { r.inRun = false }
