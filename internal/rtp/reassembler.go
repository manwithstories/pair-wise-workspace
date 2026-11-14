// Package rtp reassembles RTP packets into frames on the media plane. It
// handles 16-bit sequence wrap, duplicates and in-window reordering, seals
// frames on timestamp change or marker bit, and drops incomplete frames
// when a loss burst exceeds the gap threshold. Sealed frames are never
// reverted.
package rtp

import (
	"github.com/pion/rtp"
)

// Stats are the per-track media-plane counters.
type Stats struct {
	Gaps       int // sequence numbers declared lost
	Duplicates int // packets seen more than once
	OutOfOrder int // packets buffered ahead of the expected sequence
	Discarded  int // in-progress frames dropped after an over-threshold gap
}

// Reassembler orders packets of one track and groups them into frames.
type Reassembler struct {
	window       int
	gapThreshold int
	emit         func(pkts []*rtp.Packet)

	started  bool
	expected uint16
	pending  map[uint16]*rtp.Packet

	cur      []*rtp.Packet
	curTS    uint32
	hasCur   bool
	corrupt  bool
	dropping bool
	dropTS   uint32

	stats Stats
}

// NewReassembler builds a reassembler. window is how far ahead of the
// expected sequence a packet may be while still considered reordering
// rather than loss. gapThreshold is the loss burst length tolerated before
// the in-progress frame is dropped and counted. emit receives each sealed,
// complete frame as an ordered packet slice.
func NewReassembler(window, gapThreshold int, emit func(pkts []*rtp.Packet)) *Reassembler {
	return &Reassembler{
		window:       window,
		gapThreshold: gapThreshold,
		emit:         emit,
		pending:      make(map[uint16]*rtp.Packet),
	}
}

// Stats returns a snapshot of the current counters.
func (r *Reassembler) Stats() Stats { return r.stats }

// Add feeds one packet in arrival order.
func (r *Reassembler) Add(pkt *rtp.Packet) {
	seq := pkt.SequenceNumber
	if !r.started {
		r.started = true
		r.expected = seq
	}
	diff := int16(seq - r.expected) // signed arithmetic absorbs 16-bit wrap
	switch {
	case diff == 0:
		r.consume(pkt)
		r.expected++
		r.drain()
	case diff > 0:
		if int(diff) > r.window {
			r.settleLoss(seq)
			r.consume(pkt)
			r.expected = seq + 1
			r.drain()
			return
		}
		if _, ok := r.pending[seq]; ok {
			r.stats.Duplicates++
			return
		}
		r.pending[seq] = pkt
		r.stats.OutOfOrder++
	default: // diff < 0: already consumed, a duplicate or very late packet
		r.stats.Duplicates++
	}
}

// Flush settles the tail of the stream: sequence numbers still missing
// when the stream ends are declared lost (so their gaps are counted and
// buffered packets behind them are consumed), then any incomplete frame
// is dropped. Sealed frames already emitted are unaffected.
func (r *Reassembler) Flush() {
	if r.started && len(r.pending) > 0 {
		maxSeq := r.expected
		for s := range r.pending {
			if int16(s-maxSeq) > 0 {
				maxSeq = s
			}
		}
		r.settleLoss(maxSeq)
		r.drain()
	}
	r.cur = nil
	r.hasCur = false
	r.corrupt = false
	r.dropping = false
	r.pending = make(map[uint16]*rtp.Packet)
}

// settleLoss declares every sequence number in [expected, seq) that never
// arrived as lost, then consumes all buffered packets ahead of seq in
// order, so the caller can continue with seq itself.
func (r *Reassembler) settleLoss(seq uint16) {
	r.declareLoss(seq)
	for r.expected != seq {
		if _, ok := r.pending[r.expected]; ok {
			r.drain()
		} else {
			r.expected++
		}
	}
}

// declareLoss accounts for every sequence number in [expected, seq) that
// never arrived, then marks the in-progress frame accordingly.
func (r *Reassembler) declareLoss(seq uint16) {
	missing := 0
	for s := r.expected; s != seq; s++ {
		if _, ok := r.pending[s]; !ok {
			missing++
		}
	}
	if missing == 0 {
		return
	}
	r.stats.Gaps += missing
	if !r.hasCur {
		return
	}
	if missing > r.gapThreshold {
		// Loss burst too large: drop the incomplete frame and count it.
		r.cur = nil
		r.hasCur = false
		r.corrupt = false
		r.dropping = true
		r.dropTS = r.curTS
		r.stats.Discarded++
		return
	}
	r.corrupt = true
}

func (r *Reassembler) drain() {
	for {
		pkt, ok := r.pending[r.expected]
		if !ok {
			return
		}
		delete(r.pending, r.expected)
		r.consume(pkt)
		r.expected++
	}
}

func (r *Reassembler) consume(pkt *rtp.Packet) {
	if r.dropping {
		if pkt.Timestamp == r.dropTS {
			return // tail of a frame dropped after an over-threshold gap
		}
		r.dropping = false
	}
	if r.hasCur && pkt.Timestamp != r.curTS {
		r.seal()
	}
	if !r.hasCur {
		r.curTS = pkt.Timestamp
		r.hasCur = true
	}
	r.cur = append(r.cur, pkt)
	if pkt.Marker {
		r.seal()
	}
}

// seal closes the current frame. Complete frames are emitted and never
// reverted; corrupt ones are dropped silently.
func (r *Reassembler) seal() {
	if !r.corrupt && len(r.cur) > 0 {
		r.emit(r.cur)
	}
	r.cur = nil
	r.hasCur = false
	r.corrupt = false
}
