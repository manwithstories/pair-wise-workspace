// Package transport simulates RTP network impairments and provides the
// recovery machinery: deduplication, reordering, a bounded jitter buffer and
// deadline-driven loss concealment. All behavior is driven by a deterministic
// trace so repeated runs with the same seed are bit-identical.
package transport

import (
	"sync"

	"github.com/pion/rtp"

	"avlab/internal/media"
)

// Packet is one RTP unit qualified by the simulated arrival wall clock and
// the media epoch it belongs to.
type Packet struct {
	RTP       rtp.Packet
	Epoch     media.Epoch
	Segment   uint32
	Keyframe  bool
	ArriveTick uint64 // simulated clock in 90 kHz ticks
	Size      int
}

// Seq returns the RTP sequence number.
func (p *Packet) Seq() uint16 { return p.RTP.SequenceNumber }

// ReorderStats accumulates reordering-stage counters.
type ReorderStats struct {
	Received  uint64
	Duplicate uint64
	Reordered uint64
	LateEpoch uint64
	Released  uint64
}

// ReorderBuffer removes duplicates, sorts packets by sequence number within an
// epoch and refuses late packets from older epochs. It is safe for concurrent
// use because the jitter stage runs alongside worker pools.
type ReorderBuffer struct {
	mu      sync.Mutex
	epoch   media.Epoch
	next    uint16
	haveNext bool
	window  map[uint16]*Packet
	stats   ReorderStats
}

// NewReorderBuffer creates an empty reorder buffer.
func NewReorderBuffer() *ReorderBuffer {
	return &ReorderBuffer{window: map[uint16]*Packet{}}
}

// Push inserts one packet. A duplicate or stale-epoch packet is counted and
// dropped instead of entering the window.
func (b *ReorderBuffer) Push(p *Packet) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stats.Received++
	if p.Epoch < b.epoch {
		b.stats.LateEpoch++
		return media.NewFault(media.CodePacketLate, true,
			"packet from an expired epoch").WithSeq(p.Seq(), p.Epoch)
	}
	if p.Epoch > b.epoch {
		// discontinuity: start a fresh timeline and forget buffered packets
		b.epoch = p.Epoch
		b.window = map[uint16]*Packet{}
		b.haveNext = false
	}
	if !b.haveNext {
		b.next = p.Seq()
		b.haveNext = true
	}
	if _, exists := b.window[p.Seq()]; exists || p.Seq() == b.next-1 {
		b.stats.Duplicate++
		return media.NewFault(media.CodePacketDuplicate, true,
			"duplicate sequence number").WithSeq(p.Seq(), p.Epoch)
	}
	if seqLt(p.Seq(), b.next) {
		// already released sequence number
		b.stats.Duplicate++
		return media.NewFault(media.CodePacketDuplicate, true,
			"sequence already released").WithSeq(p.Seq(), p.Epoch)
	}
	b.window[p.Seq()] = p
	return nil
}

// CountReordered records one trace-scheduled reorder event.
func (b *ReorderBuffer) CountReordered() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stats.Reordered++
}

// CountDuplicate records one trace-scheduled duplicate event.
func (b *ReorderBuffer) CountDuplicate() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stats.Duplicate++
}

// Drain returns every packet that can be released in sequence order, stopping
// at the first remaining gap.
func (b *ReorderBuffer) Drain() []*Packet {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.haveNext {
		return nil
	}
	var out []*Packet
	for {
		p, ok := b.window[b.next]
		if !ok {
			break
		}
		out = append(out, p)
		delete(b.window, b.next)
		b.next++
	}
	b.stats.Released += uint64(len(out))
	return out
}

// AdvanceTo declares seq permanently missing (deadline expired): the buffer
// skips past it so later packets can be released.
func (b *ReorderBuffer) AdvanceTo(seq uint16) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.haveNext {
		b.next = seq
		b.haveNext = true
		return
	}
	for b.next != seq {
		delete(b.window, b.next)
		b.next++
	}
}

// Pending returns the number of buffered packets.
func (b *ReorderBuffer) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.window)
}

// Stats returns a snapshot of the counters.
func (b *ReorderBuffer) Stats() ReorderStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stats
}

// seqLt implements RFC 3550 serial number arithmetic for uint16 values.
func seqLt(a, b uint16) bool { return int16(a - b) < 0 }
