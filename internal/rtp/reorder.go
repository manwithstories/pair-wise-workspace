package rtp

import (
	"fmt"
	"sort"

	"github.com/pion/rtp"
)

// GapError is a traceable missing sequence interval for one SSRC.
type GapError struct {
	SSRC      uint32
	FirstSeq  uint16 // inclusive
	LastSeq   uint16 // inclusive
	Count     int    // number of missing sequence numbers
	AfterTick int64  // global tick of the packet that revealed the gap
	Recovered bool   // a retransmit eventually filled the interval
}

func (g GapError) Error() string {
	return fmt.Sprintf("rtp: loss ssrc=%d seq=[%d..%d] count=%d after_tick=%d",
		g.SSRC, g.FirstSeq, g.LastSeq, g.Count, g.AfterTick)
}

// OrderedPacket is an RTP packet after reordering, carrying its global tick.
type OrderedPacket struct {
	Tick   int64
	SSRC   uint32
	Seq    uint16
	Packet *rtp.Packet
}

// streamOrder tracks per-SSRC reorder state. Memory is bounded by the
// configured reorder window: only recent sequence numbers stay buffered.
type streamOrder struct {
	ssrc        uint32
	initialized bool
	expected    uint16 // next in-order sequence to emit
	highest     uint16 // highest sequence ever observed
	buffer      map[uint16]*rtp.Packet
	seen        map[uint16]bool
	gaps        []GapError
	duplicates  int
}

// Reorderer merges datagrams from several SSRCs into ticked, ordered output.
type Reorderer struct {
	streams    map[uint32]*streamOrder
	tick       int64
	maxReorder int
}

// NewReorderer builds a reorderer. maxReorder bounds how late a packet may
// arrive; beyond it the missing sequences are treated as permanent loss and
// the window advances over them.
func NewReorderer(maxReorder int) *Reorderer {
	if maxReorder <= 0 {
		maxReorder = 64
	}
	return &Reorderer{streams: map[uint32]*streamOrder{}, maxReorder: maxReorder}
}

func (r *Reorderer) stream(ssrc uint32) *streamOrder {
	s, ok := r.streams[ssrc]
	if !ok {
		s = &streamOrder{ssrc: ssrc, buffer: map[uint16]*rtp.Packet{}, seen: map[uint16]bool{}}
		r.streams[ssrc] = s
	}
	return s
}

// Push accepts one parsed RTP packet and returns every packet that becomes
// emit-ready in strict sequence order. A jump larger than maxReorder closes
// the pending gap as permanent loss and advances the window.
func (r *Reorderer) Push(p *rtp.Packet) ([]OrderedPacket, error) {
	s := r.stream(p.SSRC)
	if !s.initialized {
		s.initialized = true
		s.expected, s.highest = p.SequenceNumber, p.SequenceNumber
	}
	if s.seen[p.SequenceNumber] {
		s.duplicates++
		return nil, nil // idempotent: duplicates are dropped without side effects
	}
	lag := int16(s.highest - p.SequenceNumber)
	if lag > int16(r.maxReorder) {
		return nil, fmt.Errorf("%w: ssrc=%d seq=%d arrives %d behind high-water %d",
			ErrMalformed, p.SSRC, p.SequenceNumber, lag, s.highest)
	}
	s.seen[p.SequenceNumber] = true
	for i := range s.gaps {
		if g := &s.gaps[i]; !g.Recovered && p.SequenceNumber >= g.FirstSeq && p.SequenceNumber <= g.LastSeq {
			g.Recovered = true
		}
	}
	if int16(p.SequenceNumber-s.highest) > 0 {
		s.recordGap(p.SequenceNumber)
		s.highest = p.SequenceNumber
	}
	s.buffer[p.SequenceNumber] = p
	return r.drain(s), nil
}

// drain emits contiguous packets from expected onward; when the high-water
// mark leaves expected behind by more than the window, missing sequences are
// declared permanent loss and expected jumps to the next buffered packet.
func (r *Reorderer) drain(s *streamOrder) []OrderedPacket {
	var out []OrderedPacket
	for {
		if p, ok := s.buffer[s.expected]; ok {
			delete(s.buffer, s.expected)
			out = append(out, OrderedPacket{Tick: r.tick, SSRC: s.ssrc, Seq: s.expected, Packet: p})
			r.tick++
			s.expected++
			continue
		}
		if int16(s.highest-s.expected) > int16(r.maxReorder) {
			// Permanent loss: skip everything up to the high-water mark.
			s.expected = s.highest
			continue
		}
		return out
	}
}

func (s *streamOrder) recordGap(arrived uint16) {
	if arrived == s.highest+1 {
		return
	}
	first, last := s.highest+1, arrived-1
	s.gaps = append(s.gaps, GapError{
		SSRC: s.ssrc, FirstSeq: first, LastSeq: last,
		Count: int(int16(last-first)) + 1, AfterTick: -1,
	})
}

// Flush emits any remaining buffered packets (end of stream, no further
// retransmits possible) and returns the final gap list.
func (r *Reorderer) Flush() ([]OrderedPacket, []GapError) {
	var out []OrderedPacket
	ssrcs := make([]uint32, 0, len(r.streams))
	for ssrc := range r.streams {
		ssrcs = append(ssrcs, ssrc)
	}
	sort.Slice(ssrcs, func(i, j int) bool { return ssrcs[i] < ssrcs[j] })
	for _, ssrc := range ssrcs {
		s := r.streams[ssrc]
		seqs := make([]uint16, 0, len(s.buffer))
		for seq := range s.buffer {
			seqs = append(seqs, seq)
		}
		sort.Slice(seqs, func(i, j int) bool { return int16(seqs[i]-seqs[j]) < 0 })
		for _, seq := range seqs {
			out = append(out, OrderedPacket{Tick: r.tick, SSRC: ssrc, Seq: seq, Packet: s.buffer[seq]})
			r.tick++
		}
	}
	var gaps []GapError
	for _, ssrc := range ssrcs {
		gaps = append(gaps, r.streams[ssrc].gaps...)
	}
	return out, gaps
}

// Duplicates reports the total idempotent duplicate count across SSRCs.
func (r *Reorderer) Duplicates() int {
	n := 0
	for _, s := range r.streams {
		n += s.duplicates
	}
	return n
}
