// Package rtp reorders RTP packets and recovers from loss where
// possible: sequence wrap-around, duplicates, reordering and single or
// burst loss. Recoverable packets are re-seated by timestamp;
// unrecoverable ranges are reported as gaps on the shared clock.
package rtp

import (
	"github.com/pion/rtp"

	"fieldqc/internal/clock"
)

// Gap is an unrecoverable loss span in media time.
type Gap struct {
	StartSeq uint32      `json:"start_seq"`
	EndSeq   uint32      `json:"end_seq"` // exclusive
	Start    clock.Ratio `json:"start"`
	End      clock.Ratio `json:"end"`
}

// Stats counts packet fates for one stream.
type Stats struct {
	Received  int `json:"received"`
	Recovered int `json:"recovered"` // lost packet re-seated before declaration
	Reordered int `json:"reordered"` // arrived out of order, buffered
	Duplicate int `json:"duplicate"`
	Late      int `json:"late"` // arrived after its slot was emitted/declared lost
	Lost      int `json:"lost"` // never recovered
}

// RecoveryPct returns recovered/(recovered+lost) as a percentage.
func (s Stats) RecoveryPct() float64 {
	tot := s.Recovered + s.Lost
	if tot == 0 {
		return 100
	}
	return 100 * float64(s.Recovered) / float64(tot)
}

// Add merges another stream's stats.
func (s *Stats) Add(o Stats) {
	s.Received += o.Received
	s.Recovered += o.Recovered
	s.Reordered += o.Reordered
	s.Duplicate += o.Duplicate
	s.Late += o.Late
	s.Lost += o.Lost
}

// Repairer is a streaming reorder/loss-recovery engine for one SSRC.
type Repairer struct {
	window  int
	rate    int64
	next    uint32 // next extended sequence to emit
	started bool
	highest uint32
	pending map[uint32]*rtp.Packet
	out     []*rtp.Packet
	stats   Stats
	gaps    []Gap
	open    *Gap
	lastTS  uint32
}

// NewRepairer creates a Repairer; window is how many missing sequence
// numbers are tolerated before a loss is declared, rate is the RTP
// clock rate used to place gaps on the shared time base.
func NewRepairer(window int, clockRate uint32) *Repairer {
	if window < 1 {
		window = 1
	}
	return &Repairer{window: window, rate: int64(clockRate), pending: map[uint32]*rtp.Packet{}}
}

// extend maps a 16-bit sequence number onto the extended 32-bit axis,
// handling wrap-around relative to the highest sequence seen.
func (r *Repairer) extend(seq uint16) uint32 {
	if !r.started {
		return uint32(seq)
	}
	cand := (r.highest & 0xFFFF0000) | uint32(seq)
	if cand+0x8000 < r.highest {
		cand += 0x10000 // wrapped forward
	} else if cand >= 0x10000 && cand > r.highest+0x8000 {
		cand -= 0x10000 // late packet from the previous cycle
	}
	return cand
}

// Push feeds one received packet into the repairer.
func (r *Repairer) Push(p *rtp.Packet) {
	ext := r.extend(p.SequenceNumber)
	if !r.started {
		r.next, r.started = ext, true
	} else if ext < r.next {
		r.stats.Late++
		return
	}
	if _, dup := r.pending[ext]; dup {
		r.stats.Duplicate++
		return
	}
	r.stats.Received++
	if ext != r.next {
		r.stats.Reordered++ // out of order; buffered for re-seating
	} else if len(r.pending) > 0 {
		r.stats.Recovered++ // fills a hole that followers were waiting on
	}
	r.pending[ext] = p
	if ext > r.highest {
		r.highest = ext
	}
	r.drain()
}

func (r *Repairer) drain() {
	for {
		if p, ok := r.pending[r.next]; ok {
			delete(r.pending, r.next)
			r.emit(p)
			r.next++
			continue
		}
		if r.highest >= r.next+uint32(r.window) {
			r.declareLoss(r.next)
			r.next++
			continue
		}
		return
	}
}

// Flush emits everything still pending and closes any open gap; call it
// at end of stream.
func (r *Repairer) Flush() {
	if !r.started {
		return
	}
	for r.next <= r.highest {
		if p, ok := r.pending[r.next]; ok {
			delete(r.pending, r.next)
			r.emit(p)
		} else {
			r.declareLoss(r.next)
		}
		r.next++
	}
	r.closeGap(r.lastTS)
}

func (r *Repairer) emit(p *rtp.Packet) {
	r.out = append(r.out, p)
	r.closeGap(p.Timestamp)
	r.lastTS = p.Timestamp
}

func (r *Repairer) declareLoss(seq uint32) {
	r.stats.Lost++
	if r.open == nil {
		start, _ := clock.Ticks(int64(r.lastTS), r.rate)
		r.open = &Gap{StartSeq: seq, Start: start}
	}
	r.open.EndSeq = seq + 1
}

func (r *Repairer) closeGap(ts uint32) {
	if r.open == nil {
		return
	}
	r.open.End, _ = clock.Ticks(int64(ts), r.rate)
	r.gaps = append(r.gaps, *r.open)
	r.open = nil
}

// Take returns the packets emitted since the last Take, in order.
func (r *Repairer) Take() []*rtp.Packet {
	out := r.out
	r.out = nil
	return out
}

// Stats returns a copy of the current counters.
func (r *Repairer) Stats() Stats { return r.stats }

// Gaps returns the unrecoverable loss spans recorded so far.
func (r *Repairer) Gaps() []Gap { return append([]Gap(nil), r.gaps...) }
