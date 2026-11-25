// Package transport handles RTP sequence/timestamp deduplication, reordering,
// wraparound and gap detection, plus bandwidth-driven pacing adaptation. It
// shares the media fault model and never emits a late duplicate twice.
package transport

import (
	"fmt"

	"github.com/pion/rtp"

	"relaycheck/internal/media"
)

// Gap is a contiguous run of missing RTP sequence numbers.
type Gap struct {
	FromSeq uint16 `json:"from_seq"`
	ToSeq   uint16 `json:"to_seq"` // inclusive
	Packets int    `json:"packets"`
	StartTS uint32 `json:"start_ts"`
	EndTS   uint32 `json:"end_ts"`
}

// SeqRange renders the inclusive sequence interval.
func (g Gap) SeqRange() string { return fmt.Sprintf("%d-%d", g.FromSeq, g.ToSeq) }

type extGap struct{ from, to uint64 }

// Stats summarizes a reordering run.
type Stats struct {
	Received    int `json:"received"`
	Duplicates  int `json:"duplicates"`
	Reordered   int `json:"reordered"`
	LateDropped int `json:"late_dropped"`
	Emitted     int `json:"emitted"`
	Gaps        int `json:"gaps"`
	Wraps       int `json:"wraps"`
}

type buffered struct {
	pkt *rtp.Packet
	ext uint64
}

// ReorderBuffer is a wrap-aware sliding reorder window.
type ReorderBuffer struct {
	window      uint64
	seenKeys    map[uint64]bool
	buf         map[uint64]*buffered
	initialized bool
	baseExt     uint64
	nextExt     uint64
	maxExt      uint64
	wraps       int
	stats       Stats
	extGaps     []extGap
}

// NewReorderBuffer builds a buffer tolerating up to window out-of-order packets.
func NewReorderBuffer(window int) (*ReorderBuffer, *media.Fault) {
	if window < 0 {
		return nil, media.NewFault(media.CodeInvalidInput, "reorder window must be >= 0, got %d", window)
	}
	if window == 0 {
		window = 1
	}
	return &ReorderBuffer{
		window:   uint64(window),
		seenKeys: map[uint64]bool{},
		buf:      map[uint64]*buffered{},
	}, nil
}

// Stats returns a copy of the run statistics.
func (r *ReorderBuffer) Stats() Stats { return r.stats }

// Gaps returns detected missing sequence runs converted to uint16 ranges.
func (r *ReorderBuffer) Gaps() []Gap {
	out := make([]Gap, 0, len(r.extGaps))
	for _, g := range r.extGaps {
		out = append(out, Gap{
			FromSeq: uint16(g.from),
			ToSeq:   uint16(g.to),
			Packets: int(g.to-g.from) + 1,
		})
	}
	return out
}

// extSeq maps a uint16 sequence onto the contiguous 64-bit space near ref.
func extSeq(ref uint64, seq uint16) uint64 {
	hi := ref &^ uint64(0xffff)
	cand := hi | uint64(seq)
	const half = uint64(1) << 15
	if cand+half < ref { // wrapped forward: 65535 followed by 0
		cand += 1 << 16
	} else if cand > ref+half && cand >= 1<<16 { // wrapped backward
		cand -= 1 << 16
	}
	return cand
}

func dedupKey(p *rtp.Packet) uint64 {
	return (uint64(p.SequenceNumber) << 32) | uint64(p.Timestamp)
}

func (r *ReorderBuffer) markMissing(ext uint64) {
	if n := len(r.extGaps); n > 0 && r.extGaps[n-1].to+1 == ext {
		r.extGaps[n-1].to = ext
		return
	}
	r.extGaps = append(r.extGaps, extGap{from: ext, to: ext})
}

// Push ingests one packet and returns any packets now ready in order. An exact
// duplicate or a packet too late for the window is never returned.
func (r *ReorderBuffer) Push(p *rtp.Packet) ([]*rtp.Packet, *media.Fault) {
	if p == nil {
		return nil, media.NewFault(media.CodeInvalidInput, "nil rtp packet")
	}
	r.stats.Received++
	key := dedupKey(p)
	if r.seenKeys[key] {
		r.stats.Duplicates++
		return nil, nil
	}
	r.seenKeys[key] = true

	if !r.initialized {
		r.initialized = true
		r.baseExt = uint64(p.SequenceNumber)
		r.nextExt = r.baseExt
		r.maxExt = r.baseExt
	}
	ext := extSeq(r.maxExt, p.SequenceNumber)
	if uint16(r.maxExt) > 0xf000 && p.SequenceNumber < 0x1000 && ext > r.maxExt {
		r.wraps++
		r.stats.Wraps = r.wraps
	}
	if ext > r.maxExt {
		r.maxExt = ext
	}

	if ext < r.nextExt { // already delivered retransmission/late copy
		r.stats.LateDropped++
		return nil, nil
	}
	if r.maxExt-ext > r.window { // outside the reorder window
		r.stats.LateDropped++
		return nil, nil
	}
	if _, exists := r.buf[ext]; exists {
		r.stats.Duplicates++
		return nil, nil
	}
	if ext < r.maxExt {
		r.stats.Reordered++
	}
	r.buf[ext] = &buffered{pkt: p, ext: ext}
	return r.drain(false), nil
}

// drain emits contiguous packets. Missing heads are skipped once they have
// aged past the window, or immediately during a flush.
func (r *ReorderBuffer) drain(flush bool) []*rtp.Packet {
	var out []*rtp.Packet
	for {
		if b, ok := r.buf[r.nextExt]; ok {
			out = append(out, b.pkt)
			r.stats.Emitted++
			delete(r.buf, r.nextExt)
			r.nextExt++
			continue
		}
		if r.nextExt > r.maxExt {
			break
		}
		if !flush && r.maxExt < r.nextExt+r.window {
			break // wait for a possibly in-window packet
		}
		r.markMissing(r.nextExt)
		r.stats.Gaps = len(r.extGaps)
		r.nextExt++
	}
	return out
}

// Flush forces delivery of all buffered packets and records tail gaps.
func (r *ReorderBuffer) Flush() []*rtp.Packet {
	out := r.drain(true)
	r.stats.Gaps = len(r.extGaps)
	return out
}

// ReorderAll pushes every packet and flushes, returning ordered unique packets.
func ReorderAll(pkts []*rtp.Packet, window int) ([]*rtp.Packet, []Gap, Stats, *media.Fault) {
	rb, f := NewReorderBuffer(window)
	if f != nil {
		return nil, nil, Stats{}, f
	}
	var ordered []*rtp.Packet
	for _, p := range pkts {
		out, pf := rb.Push(p)
		if pf != nil {
			return nil, nil, rb.Stats(), pf
		}
		ordered = append(ordered, out...)
	}
	ordered = append(ordered, rb.Flush()...)
	return ordered, rb.Gaps(), rb.Stats(), nil
}
