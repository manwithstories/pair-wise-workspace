// Package transport turns the lossy RTP packet stream into an ordered,
// bounded view of the media. The jitter buffer reorders by RTP sequence
// number and suppresses duplicates; recovery.go adds local XOR FEC so a
// single missing packet can be rebuilt while a burst leaves an explicit,
// inspectable loss marker instead of being papered over.
package transport

import (
	"errors"
	"sync"

	"github.com/pion/rtp"
)

// DefaultBufferCap bounds in-flight packets. Once this many distinct
// sequence numbers are buffered, new packets are refused so a disorderly
// sender cannot grow memory without limit.
const DefaultBufferCap = 256

// ErrBufferFull means the bounded buffer cannot accept the packet yet.
var ErrBufferFull = errors.New("transport: jitter buffer capacity exceeded")

// Gap is one still-missing sequence position after a flush boundary.
type Gap struct {
	SeqNum uint16
	// Recoverable is true when local FEC can rebuild this single packet.
	Recoverable bool
}

// JitterBuffer holds RTP packets keyed by sequence number and serves them in
// order. It is safe for concurrent use; the ordering model is a sorted set
// whose lowest element becomes the delivery base.
type JitterBuffer struct {
	mu       sync.Mutex
	cap      int
	packets  map[uint16]*rtp.Packet
	base     uint16
	haveBase bool
	advanced bool // base has moved because packets were delivered/flushed
	stats    Stats
}

// Stats summarizes buffer behavior for deterministic reporting.
type Stats struct {
	Received   int
	Duplicates int
	Late       int // arrived after their position was already flushed
	Flushed    int
	MaxDepth   int
}

// NewJitterBuffer builds a buffer with the given packet capacity (<=0 =>
// DefaultBufferCap).
func NewJitterBuffer(capacity int) *JitterBuffer {
	if capacity <= 0 {
		capacity = DefaultBufferCap
	}
	return &JitterBuffer{cap: capacity, packets: map[uint16]*rtp.Packet{}}
}

// Push inserts one packet. Duplicate sequence numbers are counted and
// dropped; packets older than the already-flushed base are reported late;
// a saturated buffer returns ErrBufferFull.
func (j *JitterBuffer) Push(p *rtp.Packet) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.stats.Received++
	seq := p.SequenceNumber
	if _, exists := j.packets[seq]; exists {
		j.stats.Duplicates++
		return nil
	}
	// "Late" means behind a base that delivery has already moved past.
	// Before the first flush an earlier packet is ordinary reordering and
	// must re-anchor the base instead of being discarded.
	if j.haveBase && j.advanced && seqDistance(j.base, seq) < 0 {
		j.stats.Late++
		return nil
	}
	if len(j.packets) >= j.cap {
		return ErrBufferFull
	}
	j.packets[seq] = p
	if !j.haveBase {
		j.base, j.haveBase = seq, true
	} else if !j.advanced && seqDistance(j.base, seq) < 0 {
		// Before the first flush the base tracks the lowest buffered
		// sequence number; a later packet never moves it forward.
		j.base = seq
	}
	if depth := len(j.packets); depth > j.stats.MaxDepth {
		j.stats.MaxDepth = depth
	}
	return nil
}

// Next removes and returns the packet at the current base, or nil if that
// sequence number is still missing. The base advances only on delivery.
func (j *JitterBuffer) Next() *rtp.Packet {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.haveBase {
		return nil
	}
	p, ok := j.packets[j.base]
	if !ok {
		return nil
	}
	delete(j.packets, j.base)
	j.base++
	j.advanced = true
	j.stats.Flushed++
	return p
}

// Drain returns every contiguous packet starting at the lowest buffered
// sequence number, stopping at the first hole.
func (j *JitterBuffer) Drain() []*rtp.Packet {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.packets) == 0 {
		return nil
	}
	if !j.haveBase {
		j.base = j.lowestLocked()
		j.haveBase = true
	}
	var out []*rtp.Packet
	for {
		p, ok := j.packets[j.base]
		if !ok {
			return out
		}
		delete(j.packets, j.base)
		out = append(out, p)
		j.base++
		j.advanced = true
		j.stats.Flushed++
	}
}

// Missing reports sequence holes between the base and the highest buffered
// packet, in ascending ring order.
func (j *JitterBuffer) Missing(recoverable map[uint16]bool) []Gap {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.haveBase || len(j.packets) == 0 {
		return nil
	}
	hi := j.highestLocked()
	var gaps []Gap
	for s := j.base; seqDistance(s, hi) > 0; s++ {
		if _, ok := j.packets[s]; ok {
			continue
		}
		gaps = append(gaps, Gap{SeqNum: s, Recoverable: recoverable[s]})
	}
	return gaps
}

// Skip advances the base past an unrecoverable sequence number so the loss
// stays explicit but does not stall the stream forever.
func (j *JitterBuffer) Skip(seq uint16) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.haveBase && j.base == seq {
		j.base++
		j.advanced = true
	}
}

// Reset empties the buffer and clears the base; running stats are retained.
func (j *JitterBuffer) Reset() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.packets = map[uint16]*rtp.Packet{}
	j.haveBase = false
	j.advanced = false
}

// Base returns the current lowest expected sequence number.
func (j *JitterBuffer) Base() (uint16, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.base, j.haveBase
}

// Len returns the number of buffered packets.
func (j *JitterBuffer) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.packets)
}

// Stats returns a copy of the running counters.
func (j *JitterBuffer) Stats() Stats {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stats
}

func (j *JitterBuffer) lowestLocked() uint16 {
	low := uint16(0)
	first := true
	for seq := range j.packets {
		if first || seqDistance(seq, low) < 0 {
			low, first = seq, false
		}
	}
	return low
}

func (j *JitterBuffer) highestLocked() uint16 {
	hi := j.base
	for seq := range j.packets {
		if seqDistance(j.base, seq) > seqDistance(j.base, hi) {
			hi = seq
		}
	}
	return hi
}

// seqDistance is the signed ring distance from a to b (16-bit).
func seqDistance(a, b uint16) int32 {
	d := int32(b) - int32(a)
	if d > 32767 {
		d -= 65536
	} else if d < -32768 {
		d += 65536
	}
	return d
}
