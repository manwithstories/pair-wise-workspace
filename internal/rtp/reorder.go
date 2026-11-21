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
	AfterTick int64  // tick of the packet that revealed the gap
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
	highest     uint16 // highest sequence ever observed
	buffer      map[uint16]*rtp.Packet
	seen        map[uint16]bool
	gaps        []GapError
	duplicates  int
}

// Reorderer merges datagrams from several SSRCs into ticked output. Packets
// are emitted on arrival (loss never stalls the stream); a bounded window
// lets late packets arrive before their absence is declared permanent.
type Reorderer struct {
	streams    map[uint32]*streamOrder
	tick       int64
	maxReorder int
}

// NewReorderer builds a reorderer. maxReorder bounds how late a packet may
// arrive relative to the high-water mark before it is rejected.
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

// Push accepts one parsed RTP packet and emits it immediately with a tick.
// Duplicates return nothing and have no side effects; a packet beyond the
// reorder window is rejected with its SSRC and sequence attached.
func (r *Reorderer) Push(p *rtp.Packet) ([]OrderedPacket, error) {
	s := r.stream(p.SSRC)
	if !s.initialized {
		s.initialized = true
		s.highest = p.SequenceNumber
	}
	if s.seen[p.SequenceNumber] {
		s.duplicates++
		return nil, nil // idempotent: duplicates are dropped without side effects
	}
	if int16(s.highest-p.SequenceNumber) > int16(r.maxReorder) {
		return nil, fmt.Errorf("%w: ssrc=%d seq=%d arrives %d behind high-water %d",
			ErrMalformed, p.SSRC, p.SequenceNumber, int16(s.highest-p.SequenceNumber), s.highest)
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
	op := OrderedPacket{Tick: r.tick, SSRC: p.SSRC, Seq: p.SequenceNumber, Packet: p}
	r.tick++
	r.evict(s)
	return []OrderedPacket{op}, nil
}

// evict drops buffered packets beyond the window; their gaps stay recorded as
// permanent loss. Buffering is only used for gap/late accounting here.
func (r *Reorderer) evict(s *streamOrder) {
	for seq := range s.buffer {
		if int16(s.highest-seq) > int16(r.maxReorder) {
			delete(s.buffer, seq)
		}
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

// Flush reports final gap statistics; all packets were already emitted.
func (r *Reorderer) Flush() ([]OrderedPacket, []GapError) {
	ssrcs := make([]uint32, 0, len(r.streams))
	for ssrc := range r.streams {
		ssrcs = append(ssrcs, ssrc)
	}
	sort.Slice(ssrcs, func(i, j int) bool { return ssrcs[i] < ssrcs[j] })
	var gaps []GapError
	for _, ssrc := range ssrcs {
		gaps = append(gaps, r.streams[ssrc].gaps...)
	}
	return nil, gaps
}

// Duplicates reports the total idempotent duplicate count across SSRCs.
func (r *Reorderer) Duplicates() int {
	n := 0
	for _, s := range r.streams {
		n += s.duplicates
	}
	return n
}
