// Package transport reorders RTP packets, identifies loss bursts and
// recovers lost packets from delayed redundancy copies. Unrecoverable
// losses are reported as gaps; media time is never fabricated.
package transport

import (
	"encoding/binary"
	"fmt"

	"github.com/pion/rtp"

	"medrelay/internal/timeline"
)

// Redundancy marker appended to payloads: 0xFE 'R' | uint32 orig timestamp |
// uint16 payload length | payload of the packet RedundancyStride earlier.
var redMark = []byte{0xFE, 'R'}

// Config bounds the reorder window and describes the redundancy scheme.
type Config struct {
	MaxQueue         int    // max buffered out-of-order packets
	RedundancyStride uint16 // packet seq distance of the redundant copy
}

// Gap is an unrecoverable run of sequence numbers. Timestamps are the
// bounds observed on neighbouring packets, not invented media time.
type Gap struct {
	FromSeq, ToSeq uint16
	StartTS, EndTS uint32
}

// Stats summarises recovery results.
type Stats struct {
	Received   int
	Lost       int
	Recovered  int
}

// RecoveryRate is recovered/lost; 1 when nothing was lost.
func (s Stats) RecoveryRate() float64 {
	if s.Lost == 0 {
		return 1
	}
	return float64(s.Recovered) / float64(s.Lost)
}

// Reorderer is a bounded reorder buffer with redundancy recovery.
type Reorderer struct {
	cfg     Config
	buf     map[uint16]*rtp.Packet
	head    uint16
	maxSeen uint16
	started bool
	lastTS  uint32
	stats   Stats
	gaps    []Gap
}

func NewReorderer(cfg Config) *Reorderer {
	if cfg.MaxQueue <= 0 {
		cfg.MaxQueue = 256
	}
	return &Reorderer{cfg: cfg, buf: map[uint16]*rtp.Packet{}}
}

func seqLess(a, b uint16) bool { return int16(a-b) < 0 }

// Stats returns accumulated recovery statistics.
func (r *Reorderer) Stats() Stats { return r.stats }

// Gaps returns the unrecoverable gaps declared so far.
func (r *Reorderer) Gaps() []Gap { return r.gaps }

// Push buffers one packet and emits every packet that became contiguous,
// in sequence order. Recovered packets are emitted in their original
// position with their original timestamp.
func (r *Reorderer) Push(p *rtp.Packet) ([]*rtp.Packet, error) {
	if !r.started {
		r.head, r.started = p.SequenceNumber, true
	}
	if seqLess(p.SequenceNumber, r.head) {
		return nil, nil // duplicate or late; already emitted or gapped
	}
	// Reject input that jumps further ahead of the newest packet seen
	// than the reorder window: the queue is bounded and such a burst
	// can never be reordered.
	if r.stats.Received > 0 && seqLess(r.maxSeen, p.SequenceNumber) &&
		p.SequenceNumber-r.maxSeen > uint16(r.cfg.MaxQueue) {
		return nil, timeline.NewSeq(timeline.ErrLimit, "", uint64(p.SequenceNumber), 0, 0,
			fmt.Sprintf("packet beyond reorder window of %d", r.cfg.MaxQueue))
	}
	if seqLess(r.maxSeen, p.SequenceNumber) || r.stats.Received == 0 {
		r.maxSeen = p.SequenceNumber
	}
	if _, dup := r.buf[p.SequenceNumber]; dup {
		return nil, nil
	}
	r.stats.Received++
	r.buf[p.SequenceNumber] = p
	if len(r.buf) > r.cfg.MaxQueue {
		return nil, timeline.NewSeq(timeline.ErrLimit, "", uint64(p.SequenceNumber), 0, 0,
			fmt.Sprintf("reorder queue exceeds %d packets", r.cfg.MaxQueue))
	}
	return r.emit(), nil
}

func (r *Reorderer) emit() []*rtp.Packet {
	var out []*rtp.Packet
	for {
		if p, ok := r.buf[r.head]; ok {
			delete(r.buf, r.head)
			r.lastTS = p.Timestamp
			out = append(out, p)
			r.head++
			continue
		}
		if rec, ok := r.tryRecover(r.head); ok {
			r.stats.Recovered++
			r.stats.Lost++
			r.lastTS = rec.Timestamp
			out = append(out, rec)
			r.head++
			continue
		}
		// Declare a gap only when the window is full of later packets.
		if len(r.buf) < r.cfg.MaxQueue {
			break
		}
		from := r.head
		for r.buf[r.head] == nil {
			if _, ok := r.tryRecover(r.head); ok {
				break
			}
			r.head++
			if r.head == from { // wrapped all the way around
				break
			}
		}
		gap := Gap{FromSeq: from, ToSeq: r.head - 1, StartTS: r.lastTS}
		if p, ok := r.buf[r.head]; ok {
			gap.EndTS = p.Timestamp
		}
		r.stats.Lost += int(r.head - from)
		r.gaps = append(r.gaps, gap)
	}
	return out
}

// tryRecover rebuilds the packet with the given sequence number from the
// redundant copy carried by the packet RedundancyStride later.
func (r *Reorderer) tryRecover(seq uint16) (*rtp.Packet, bool) {
	if r.cfg.RedundancyStride == 0 {
		return nil, false
	}
	carrier, ok := r.buf[seq+r.cfg.RedundancyStride]
	if !ok {
		return nil, false
	}
	ts, payload, ok := ExtractRedundant(carrier.Payload)
	if !ok {
		return nil, false
	}
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           carrier.SSRC,
			PayloadType:    carrier.PayloadType,
		},
		Payload: payload,
	}, true
}

// Drain flushes the buffer at end of stream, declaring gaps for anything
// still missing.
func (r *Reorderer) Drain() []*rtp.Packet {
	saved := r.cfg.MaxQueue
	r.cfg.MaxQueue = 1
	out := r.emit()
	for len(r.buf) > 0 {
		from := r.head
		for r.buf[r.head] == nil && r.head != from-1 {
			r.head++
		}
		if from != r.head {
			r.stats.Lost += int(r.head - from)
			r.gaps = append(r.gaps, Gap{FromSeq: from, ToSeq: r.head - 1, StartTS: r.lastTS})
		}
		if p, ok := r.buf[r.head]; ok {
			delete(r.buf, r.head)
			r.lastTS = p.Timestamp
			out = append(out, p)
			r.head++
		}
	}
	r.cfg.MaxQueue = saved
	return out
}

// AppendRedundant returns payload with a redundant copy of an earlier
// packet's timestamp and payload appended.
func AppendRedundant(payload []byte, origTS uint32, orig []byte) []byte {
	out := make([]byte, 0, len(payload)+8+len(orig))
	out = append(out, payload...)
	out = append(out, redMark...)
	var tmp [6]byte
	binary.BigEndian.PutUint32(tmp[0:4], origTS)
	binary.BigEndian.PutUint16(tmp[4:6], uint16(len(orig)))
	out = append(out, tmp[:]...)
	return append(out, orig...)
}

// ExtractRedundant splits a payload into main part and redundant copy.
// ok is false when no redundancy marker is present.
func ExtractRedundant(payload []byte) (ts uint32, redundant []byte, ok bool) {
	for i := 0; i+8 <= len(payload); i++ {
		if payload[i] == redMark[0] && payload[i+1] == redMark[1] {
			ts = binary.BigEndian.Uint32(payload[i+2 : i+6])
			n := int(binary.BigEndian.Uint16(payload[i+6 : i+8]))
			if i+8+n != len(payload) {
				return 0, nil, false
			}
			return ts, payload[i+8:], true
		}
	}
	return 0, nil, false
}

// MainPayload strips the redundancy trailer, returning the main payload.
func MainPayload(payload []byte) []byte {
	for i := 0; i+8 <= len(payload); i++ {
		if payload[i] == redMark[0] && payload[i+1] == redMark[1] {
			n := int(binary.BigEndian.Uint16(payload[i+6 : i+8]))
			if i+8+n == len(payload) {
				return payload[:i]
			}
		}
	}
	return payload
}
