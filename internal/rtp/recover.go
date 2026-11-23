// Package rtp simulates RTP transport faults (burst loss, duplication,
// reordering) and recovers packets via a jitter buffer plus retransmission.
package rtp

import (
	"github.com/pion/rtp"
)

// Stats summarizes recovery for one channel.
type Stats struct {
	Sent       int
	Lost       int // gaps detected on the wire
	Recovered  int // gaps filled by retransmission
	Duplicates int
	Reordered  int
	LatencyMS  []float64 // per-released-packet latency model
}

// RecoveryRate is the fraction of lost packets recovered (1.0 if none lost).
func (s Stats) RecoveryRate() float64 {
	if s.Lost == 0 {
		return 1
	}
	return float64(s.Recovered) / float64(s.Lost)
}

// Unrecovered is the number of gaps never filled.
func (s Stats) Unrecovered() int { return s.Lost - s.Recovered }

// Packetize chunks a payload into RTP packets of at most mtu payload bytes,
// setting the marker bit on the last packet of the unit.
func Packetize(payload []byte, ssrc uint32, seq *uint16, ts uint32, mtu int) []rtp.Packet {
	if mtu <= 0 {
		mtu = 1200
	}
	var pkts []rtp.Packet
	for off := 0; off < len(payload) || (off == 0 && len(payload) == 0); {
		end := off + mtu
		if end > len(payload) {
			end = len(payload)
		}
		p := rtp.Packet{
			Header: rtp.Header{
				Version: 2, SequenceNumber: *seq, Timestamp: ts, SSRC: ssrc,
				Marker: end == len(payload),
			},
			Payload: payload[off:end],
		}
		pkts = append(pkts, p)
		*seq++
		off = end
		if len(payload) == 0 {
			break
		}
	}
	return pkts
}

// Wire is the result of simulating a lossy channel.
type Wire struct {
	Packets []rtp.Packet   // what the receiver sees (loss, dups, reorder applied)
	Lost    map[uint16]bool // sequence numbers dropped on the wire
}

// Simulate transmits pkts over a lossy channel: a burst of burstLen drops
// starting at burstStart, plus deterministic duplication and reordering.
func Simulate(pkts []rtp.Packet, burstStart, burstLen int, dupEvery, reorderEvery int) Wire {
	w := Wire{Lost: map[uint16]bool{}}
	for i, p := range pkts {
		if i >= burstStart && i < burstStart+burstLen {
			w.Lost[p.SequenceNumber] = true
			continue
		}
		w.Packets = append(w.Packets, p)
		if dupEvery > 0 && i%dupEvery == dupEvery/2 {
			w.Packets = append(w.Packets, p) // duplicate
		}
	}
	// Reorder: swap adjacent pairs deterministically.
	if reorderEvery > 0 {
		for i := reorderEvery - 1; i+1 < len(w.Packets); i += reorderEvery {
			w.Packets[i], w.Packets[i+1] = w.Packets[i+1], w.Packets[i]
		}
	}
	return w
}

// Recoverer is a jitter buffer with retransmission-based gap recovery.
type Recoverer struct {
	src        map[uint16]rtp.Packet // sender-side buffer for retransmission
	buf        map[uint16]rtp.Packet
	released   map[uint16]bool
	next       uint16
	started    bool
	maxSeq     uint16
	depth      int
	pktMS      float64
	rttMS      float64
	Stats      Stats
}

// NewRecoverer builds a Recoverer. src is the sender buffer used to answer
// retransmission requests; packets absent from src are unrecoverable.
func NewRecoverer(src []rtp.Packet, depth int, pktMS, rttMS float64) *Recoverer {
	r := &Recoverer{
		src:      map[uint16]rtp.Packet{},
		buf:      map[uint16]rtp.Packet{},
		released: map[uint16]bool{},
		depth:    depth,
		pktMS:    pktMS,
		rttMS:    rttMS,
	}
	for _, p := range src {
		r.src[p.SequenceNumber] = p
	}
	r.Stats.Sent = len(src)
	return r
}

// Push feeds one received packet into the jitter buffer.
func (r *Recoverer) Push(p rtp.Packet) {
	s := p.SequenceNumber
	if r.released[s] {
		r.Stats.Duplicates++
		return
	}
	if _, dup := r.buf[s]; dup {
		r.Stats.Duplicates++
		return
	}
	if !r.started {
		r.next, r.maxSeq, r.started = s, s, true
	} else {
		if seqLess(s, r.maxSeq) {
			r.Stats.Reordered++
		} else {
			r.maxSeq = s
		}
	}
	r.buf[s] = p
}

// Drain releases all currently releasable in-order packets, recovering gaps
// once the buffer holds more than depth packets beyond them.
func (r *Recoverer) Drain() []rtp.Packet {
	var out []rtp.Packet
	for {
		if p, ok := r.buf[r.next]; ok {
			out = append(out, r.release(p))
			continue
		}
		// Gap: only recover once enough later packets have arrived.
		if len(r.buf) <= r.depth {
			break
		}
		pkt, recovered, progress := r.recoverGap()
		if !progress {
			break
		}
		if recovered {
			out = append(out, pkt)
		}
	}
	return out
}

// Finish flushes the buffer at end of stream, recovering remaining gaps.
func (r *Recoverer) Finish() []rtp.Packet {
	var out []rtp.Packet
	for len(r.buf) > 0 {
		if p, ok := r.buf[r.next]; ok {
			out = append(out, r.release(p))
			continue
		}
		pkt, recovered, progress := r.recoverGap()
		if !progress {
			break
		}
		if recovered {
			out = append(out, pkt)
		}
	}
	return out
}

func (r *Recoverer) release(p rtp.Packet) rtp.Packet {
	delete(r.buf, p.SequenceNumber)
	r.released[p.SequenceNumber] = true
	r.next = p.SequenceNumber + 1
	r.Stats.LatencyMS = append(r.Stats.LatencyMS, float64(r.depth)*r.pktMS)
	return p
}

// recoverGap tries to fill r.next by retransmission. It returns the
// recovered packet (when found), whether the gap was recovered, and whether
// any progress was made.
func (r *Recoverer) recoverGap() (pkt rtp.Packet, recovered, progress bool) {
	gap := r.next
	if _, beyond := r.anyBeyond(gap); !beyond {
		return pkt, false, false
	}
	r.Stats.Lost++
	if p, ok := r.src[gap]; ok {
		r.Stats.Recovered++
		r.Stats.LatencyMS = append(r.Stats.LatencyMS, float64(r.depth)*r.pktMS+r.rttMS)
		r.released[gap] = true
		r.next = gap + 1
		return p, true, true
	}
	// Unrecoverable: skip the gap, bounded by the sender buffer.
	r.released[gap] = true
	r.next = gap + 1
	return pkt, false, true
}

func (r *Recoverer) anyBeyond(seq uint16) (uint16, bool) {
	for s := range r.buf {
		if seqLess(seq, s) {
			return s, true
		}
	}
	return 0, false
}

// seqLess reports whether a precedes b in 16-bit sequence space.
func seqLess(a, b uint16) bool { return int16(a-b) < 0 }
