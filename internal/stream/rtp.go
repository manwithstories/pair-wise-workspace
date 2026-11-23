package stream

import (
	"github.com/pion/rtp"

	"archive/internal/media"
)

// RTPStats summarizes reordering and loss recovery.
type RTPStats struct {
	Received    int // packets accepted into the reorder path
	Duplicates  int // repeated sequence numbers, dropped
	Late        int // arrived after the window passed them, dropped
	Recovered   int // hole-filling packets delivered within the window
	Unrecovered int // gaps the window slid past: unrecovered loss
	Delivered   int // packets emitted in order
}

// RecoveryRate is Recovered / (Recovered + Unrecovered): every injected
// loss is either recovered inside the window or counted unrecovered.
func (s RTPStats) RecoveryRate() float64 {
	if t := s.Recovered + s.Unrecovered; t > 0 {
		return float64(s.Recovered) / float64(t)
	}
	return 1
}

type Reorderer struct {
	window        uint16
	next, maxSeen uint16
	started       bool
	buf           map[uint16]*rtp.Packet
	cand, seen    map[uint16]bool // hole-fill candidates; recent seqs
	Stats         RTPStats
}

// NewReorderer creates a reorderer with the given window size.
func NewReorderer(window uint16) *Reorderer {
	return &Reorderer{window: window, buf: map[uint16]*rtp.Packet{},
		cand: map[uint16]bool{}, seen: map[uint16]bool{}}
}
func EncodePacket(seq uint16, timestamp uint32, payload []byte) ([]byte, error) {
	p := &rtp.Packet{Header: rtp.Header{
		Version: 2, SequenceNumber: seq, Timestamp: timestamp, SSRC: 0x15532,
	}, Payload: payload}
	b, err := p.Marshal()
	if err != nil {
		return nil, media.New(media.KindInternal, "stream.rtp", "", int64(seq), "marshal rtp", err)
	}
	return b, nil
}

// DecodePacket unmarshals one RTP packet.
func DecodePacket(b []byte) (*rtp.Packet, error) {
	p := &rtp.Packet{}
	if err := p.Unmarshal(b); err != nil {
		return nil, media.Corruptf("stream.rtp", "", -1, "unmarshal rtp: "+err.Error())
	}
	return p, nil
}

// Push ingests one packet and returns any packets now deliverable in
// sequence order.
func (r *Reorderer) Push(p *rtp.Packet) []*rtp.Packet {
	seq := p.SequenceNumber
	r.Stats.Received++
	if !r.started {
		r.next, r.maxSeen, r.started = seq, seq, true
	}
	if seq == r.next {
		if behind(seq, r.maxSeen) {
			r.Stats.Recovered++ // hole filled exactly at the delivery point
		}
		return r.deliver(p)
	}
	if behind(seq, r.next) {
		if r.seen[seq] {
			r.Stats.Duplicates++
		} else {
			r.Stats.Late++
		}
		return nil
	}
	if _, dup := r.buf[seq]; dup {
		r.Stats.Duplicates++
		return nil
	}
	// A packet arriving behind the furthest seq seen fills a hole: it is a
	// recovery candidate (retransmission within the window).
	if behind(seq, r.maxSeen) {
		r.cand[seq] = true
	} else {
		r.maxSeen = seq
	}
	r.buf[seq] = p
	r.seen[seq] = true
	return r.drain()
}

// Flush ends the stream: buffered packets are delivered in order and every
// remaining gap is counted as unrecovered loss.
func (r *Reorderer) Flush() []*rtp.Packet {
	var out []*rtp.Packet
	for len(r.buf) > 0 {
		if p, ok := r.buf[r.next]; ok {
			delete(r.buf, r.next)
			delete(r.cand, r.next)
			out = append(out, p)
			r.Stats.Delivered++
		} else {
			r.Stats.Unrecovered++
		}
		r.next++
	}
	return out
}

// deliver emits a just-arrived in-order packet and drains the buffer.
func (r *Reorderer) deliver(p *rtp.Packet) []*rtp.Packet {
	r.Stats.Delivered++
	r.seen[p.SequenceNumber] = true
	if !behind(p.SequenceNumber, r.maxSeen) {
		r.maxSeen = p.SequenceNumber
	}
	r.next++
	return append([]*rtp.Packet{p}, r.drain()...)
}

// drain emits consecutive buffered packets; if the buffer holds a packet
// farther than the window ahead, the gap at next is declared unrecovered.
func (r *Reorderer) drain() []*rtp.Packet {
	var out []*rtp.Packet
	for {
		if p, ok := r.buf[r.next]; ok {
			delete(r.buf, r.next)
			out = append(out, p)
			r.Stats.Delivered++
			if r.cand[r.next] {
				delete(r.cand, r.next)
				r.Stats.Recovered++
			}
			r.next++
			continue
		}
		if len(r.buf) > 0 && r.maxAhead() > r.window {
			r.Stats.Unrecovered++
			r.next++
			continue
		}
		r.pruneSeen()
		return out
	}
}

// maxAhead returns how far the furthest buffered packet is ahead of next.
func (r *Reorderer) maxAhead() uint16 {
	var max uint16
	for seq := range r.buf {
		if d := seq - r.next; d < 0x8000 && d > max {
			max = d
		}
	}
	return max
}

// pruneSeen drops seen markers more than a window behind next, bounding
// memory regardless of stream length.
func (r *Reorderer) pruneSeen() {
	for s := range r.seen {
		if d := r.next - s; d > r.window && d < 0x8000 {
			delete(r.seen, s)
		}
	}
}
func behind(seq, ref uint16) bool {
	d := ref - seq
	return d != 0 && d < 0x8000
}
