// Package packet deduplicates and reorders RTP packets into a clean
// per-track sequence, recording sequence gaps for the recovery module.
package packet

import (
	"encoding/binary"
	"hash/fnv"

	"github.com/pion/rtp"

	"mediaroute/internal/model"
)

// Gap is an inclusive range of missing extended sequence numbers.
type Gap struct {
	From, To uint64
	Track    string
}

// Stats summarises a reorder run.
type Stats struct {
	Duplicates int
	Reordered  int
	Gaps       []Gap
}

// Reorderer is a per-track sliding-window RTP reorderer. It handles
// 16-bit sequence wrap via extended sequence numbers.
type Reorderer struct {
	track    string
	window   int
	started  bool
	highest  uint64 // highest extended seq arrived
	expected uint64 // next extended seq to emit
	pending  map[uint64]*rtp.Packet
	gapSet   map[uint64]bool
	emitted  map[uint64]uint64 // ext seq -> payload hash
	stats    Stats
}

func NewReorderer(track string, window int) *Reorderer {
	return &Reorderer{track: track, window: window,
		pending: map[uint64]*rtp.Packet{}, gapSet: map[uint64]bool{},
		emitted: map[uint64]uint64{}}
}

// extend maps a 16-bit sequence number to its extended value near ref.
func extend(seq uint16, ref uint64) uint64 {
	ext := (ref &^ 0xFFFF) | uint64(seq)
	if ext+0x8000 < ref {
		ext += 0x10000
	} else if ext > ref+0x8000 {
		ext -= 0x10000
	}
	return ext
}

func payloadHash(b []byte) uint64 {
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}

// Push feeds one packet and returns packets newly emitted in order.
func (r *Reorderer) Push(p *rtp.Packet) ([]*rtp.Packet, error) {
	if !r.started {
		r.started = true
		r.highest, r.expected = uint64(p.SequenceNumber), uint64(p.SequenceNumber)
	}
	ext := extend(p.SequenceNumber, r.highest)
	if ext < r.expected {
		if r.gapSet[ext] { // late arrival filling a recorded gap
			delete(r.gapSet, ext)
			r.emitted[ext] = payloadHash(p.Payload)
			r.stats.Reordered++
			return []*rtp.Packet{p}, nil
		}
		if _, ok := r.emitted[ext]; ok {
			return nil, r.checkDuplicate(ext, p)
		}
		// Older than the first observed packet (stream-start race):
		// never emitted, never gapped — emit out of order.
		r.emitted[ext] = payloadHash(p.Payload)
		r.stats.Reordered++
		return []*rtp.Packet{p}, nil
	}
	if ext > r.highest {
		r.highest = ext
	} else if ext < r.highest {
		r.stats.Reordered++ // arrived behind a higher sequence number
	}
	if _, ok := r.pending[ext]; ok {
		return nil, r.checkDuplicate(ext, p)
	}
	r.pending[ext] = p
	return r.drain(), nil
}

func (r *Reorderer) checkDuplicate(ext uint64, p *rtp.Packet) error {
	h := payloadHash(p.Payload)
	prev, seen := r.emitted[ext]
	if !seen {
		if q, ok := r.pending[ext]; ok {
			prev, seen = payloadHash(q.Payload), true
		}
	}
	if seen && prev != h {
		return &model.MediaError{Code: model.ErrDuplicate, Module: "packet",
			Track: r.track, Seq: int64(ext), Timestamp: int64(p.Timestamp),
			Msg: "duplicate sequence with conflicting payload"}
	}
	r.stats.Duplicates++
	return nil
}

// drain emits in-order packets; when the window overflows it skips the
// missing sequence, recording a gap.
func (r *Reorderer) drain() []*rtp.Packet {
	var out []*rtp.Packet
	for {
		if p, ok := r.pending[r.expected]; ok {
			delete(r.pending, r.expected)
			r.emitted[r.expected] = payloadHash(p.Payload)
			out = append(out, p)
			r.expected++
			continue
		}
		if len(r.pending) <= r.window {
			return out
		}
		r.gapSet[r.expected] = true // skip a hole
		r.expected++
	}
}

// Flush emits everything still buffered in sequence order and returns
// the final statistics. Remaining holes become recorded gaps.
func (r *Reorderer) Flush() ([]*rtp.Packet, Stats) {
	var out []*rtp.Packet
	for len(r.pending) > 0 {
		if p, ok := r.pending[r.expected]; ok {
			delete(r.pending, r.expected)
			r.emitted[r.expected] = payloadHash(p.Payload)
			out = append(out, p)
		} else {
			r.gapSet[r.expected] = true
		}
		r.expected++
	}
	var gaps []Gap
	for ext := range r.gapSet {
		gaps = append(gaps, Gap{From: ext, To: ext, Track: r.track})
	}
	// coalesce adjacent singletons
	for i := 0; i < len(gaps); i++ {
		for j := i + 1; j < len(gaps); j++ {
			if gaps[j].From < gaps[i].From {
				gaps[i], gaps[j] = gaps[j], gaps[i]
			}
		}
	}
	merged := gaps[:0]
	for _, g := range gaps {
		if len(merged) > 0 && g.From == merged[len(merged)-1].To+1 {
			merged[len(merged)-1].To = g.To
		} else {
			merged = append(merged, g)
		}
	}
	r.stats.Gaps = merged
	return out, r.stats
}

// Marshal serialises a packet deterministically for timeline writes.
func Marshal(p *rtp.Packet) []byte {
	b, err := p.Marshal()
	if err != nil {
		return nil
	}
	return b
}

// SeqOf returns the raw 16-bit sequence of a marshalled packet.
func SeqOf(raw []byte) uint16 { return binary.BigEndian.Uint16(raw[2:4]) }
