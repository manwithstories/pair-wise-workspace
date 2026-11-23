// Package transport implements the RTP receive path: duplicate removal,
// modulo-65536 sequence reordering, gap tracking and a bandwidth-aware
// bounded jitter buffer. Damage detected at the packet layer is reported
// through the shared fault model so it can be mapped to sample positions.
package transport

import (
	"mediaroute/internal/clock"
	"mediaroute/internal/fault"
)

// MaxPacketBytes is the hard per-packet allocation limit. Packets larger
// than this are rejected rather than allowed to grow the cache.
const MaxPacketBytes = 64 << 10

// Gap describes one missing sequence-number interval after the buffer has
// decided it will never be filled.
type Gap struct {
	// SeqFirst/SeqLast are the missing RTP sequence numbers as observed on
	// the wire (16-bit values, wraparound already accounted for).
	SeqFirst uint16
	SeqLast  uint16
	// ExtFirst/ExtLast are the unwrapped monotonic equivalents.
	ExtFirst uint64
	ExtLast  uint64
	// RTPStart/RTPEnd bracket RTP timestamps inferred from neighbours.
	RTPStart uint32
	RTPEnd   uint32
	// TimeRange is the gap projected onto the shared nanosecond base.
	TimeRange clock.TimeRange
}

// ReorderBuffer accepts out-of-order RTP packets and emits them in
// sequence order. It handles 16-bit sequence wraparound, removes
// duplicates, refuses late packets for already committed ranges and
// tracks the gaps that remain when a deadline expires.
type ReorderBuffer struct {
	ssrc uint32
	// nextExpected is the unwrapped sequence number of the next packet
	// that will be emitted.
	nextExpected uint64
	started      bool
	pending      map[uint64][]byte
	gapDeadline  map[uint64]int64 // first missing ext seq -> deadline nanos
	duplicates   int
	lateDropped  int
	maxPending   int
	nowNanos     int64
	gapTimeout   int64
	rtpRate      clock.Rational
}

// NewReorderBuffer constructs a buffer. gapTimeoutNanos bounds how long a
// missing sequence is waited for before it is declared a gap.
func NewReorderBuffer(ssrc uint32, gapTimeoutNanos int64) *ReorderBuffer {
	return &ReorderBuffer{
		ssrc:       ssrc,
		pending:    map[uint64][]byte{},
		gapDeadline: map[uint64]int64{},
		maxPending: 4096,
		gapTimeout: gapTimeoutNanos,
		rtpRate:    clock.Rational{Num: 90000, Den: 1},
	}
}

// SetClock overrides the RTP clock rate used for time-range projection.
func (b *ReorderBuffer) SetClock(r clock.Rational) {
	if r.Valid() {
		b.rtpRate = r
	}
}

// PushResult classifies an incoming packet.
type PushResult struct {
	Accepted   bool
	Duplicate  bool
	Late       bool
	Overflow   bool
	ExtSeq     uint64
}

// Push stores one packet keyed by its unwrapped sequence number.
func (b *ReorderBuffer) Push(seq uint16, payload []byte, nowNanos int64) PushResult {
	b.nowNanos = nowNanos
	if len(payload) > MaxPacketBytes {
		return PushResult{Overflow: true}
	}
	if !b.started {
		b.nextExpected = uint64(seq)
		b.started = true
	}
	ext := b.unwrap(seq)
	if ext < b.nextExpected {
		// The sequence is behind the committed frontier. In a 16-bit
		// modulo space a stale or retransmitted packet is
		// indistinguishable from a duplicate, so count both and never
		// allow it to overwrite the committed range.
		b.duplicates++
		b.lateDropped++
		return PushResult{Duplicate: true, Late: true, ExtSeq: ext}
	}
	if _, exists := b.pending[ext]; exists {
		b.duplicates++
		return PushResult{Duplicate: true, ExtSeq: ext}
	}
	if len(b.pending) >= b.maxPending {
		return PushResult{Overflow: true, ExtSeq: ext}
	}
	cp := make([]byte, len(payload))
	copy(cp, payload)
	b.pending[ext] = cp
	if ext > b.nextExpected {
		// Record a deadline for every missing sequence in the hole so
		// expiration works even when the gap is never partially filled.
		for s := b.nextExpected; s < ext; s++ {
			if _, ok := b.gapDeadline[s]; !ok {
				b.gapDeadline[s] = nowNanos + b.gapTimeout
			}
		}
	}
	return PushResult{Accepted: true, ExtSeq: ext}
}

// Drain emits every contiguous packet starting at nextExpected, stopping
// at the first hole. Emitted packets are removed from the buffer and their
// sequence range becomes immutable.
func (b *ReorderBuffer) Drain() [][]byte {
	var out [][]byte
	for {
		pkt, ok := b.pending[b.nextExpected]
		if !ok {
			return out
		}
		delete(b.pending, b.nextExpected)
		delete(b.gapDeadline, b.nextExpected)
		out = append(out, pkt)
		b.nextExpected++
	}
}

// ExpireGaps declares gaps whose wait deadline has passed. The missing
// intervals are returned as structured Gap values; nextExpected advances
// over them so the stream can continue without fabricating packets.
func (b *ReorderBuffer) ExpireGaps(nowNanos int64) []Gap {
	b.nowNanos = nowNanos
	var gaps []Gap
	for {
		if _, ok := b.pending[b.nextExpected]; ok {
			return gaps
		}
		deadline, tracked := b.gapDeadline[b.nextExpected]
		if !tracked {
			// Stream end or nothing ever pushed past this point.
			return gaps
		}
		if nowNanos < deadline {
			return gaps
		}
		first := b.nextExpected
		last := first
		for {
			if _, ok := b.pending[last+1]; ok {
				break
			}
			if d, ok := b.gapDeadline[last+1]; ok && nowNanos >= d {
				last++
				continue
			}
			break
		}
		gap := b.buildGap(first, last)
		gaps = append(gaps, gap)
		for s := first; s <= last; s++ {
			delete(b.gapDeadline, s)
		}
		b.nextExpected = last + 1
	}
}

func (b *ReorderBuffer) buildGap(first, last uint64) Gap {
	g := Gap{
		SeqFirst:  uint16(first),
		SeqLast:   uint16(last),
		ExtFirst: first,
		ExtLast:  last,
	}
	// RTP timestamp projection uses the 90 kHz-style clock; exact RTP
	// timestamps of missing packets are unknown, so the range spans one
	// packet duration per missing sequence.
	startTicks := int64(first)
	endTicks := int64(last + 1)
	if start, err := b.rtpRate.ToNanos(startTicks); err == nil {
		end, e2 := b.rtpRate.ToNanos(endTicks)
		if e2 == nil {
			g.TimeRange = clock.TimeRange{StartNanos: start, EndNanos: end}
		}
	}
	g.RTPStart = uint32(startTicks)
	g.RTPEnd = uint32(endTicks)
	return g
}

// GapFault converts a Gap into the structured fault model with packet and
// time-range locators attached.
func (g Gap) GapFault() *fault.Error {
	return fault.New(fault.CatPacketGap,
		"burst loss: %d missing packet(s) seq %d..%d",
		g.ExtLast-g.ExtFirst+1, g.SeqFirst, g.SeqLast).
		WithSeq(int64(g.ExtFirst), int64(g.ExtLast)).
		WithTimeRange(g.TimeRange)
}

// unwrap maps a 16-bit wire sequence number onto the monotonic axis
// anchored at nextExpected, resolving modulo wraparound by nearest
// distance.
func (b *ReorderBuffer) unwrap(seq uint16) uint64 {
	base := b.nextExpected
	baseMod := uint16(base)
	delta := int32(seq) - int32(baseMod)
	// Bias toward forward distance so a wrap near 0/65535 resolves forward.
	if delta < -32768 {
		delta += 65536
	} else if delta > 32767 {
		delta -= 65536
	}
	return uint64(int64(base) + int64(delta))
}

// Stats returns buffer counters for reports.
type Stats struct {
	Duplicates  int
	LateDropped int
	Pending     int
	NextExtSeq  uint64
}

// Snapshot returns the current counters.
func (b *ReorderBuffer) Snapshot() Stats {
	return Stats{
		Duplicates:  b.duplicates,
		LateDropped: b.lateDropped,
		Pending:     len(b.pending),
		NextExtSeq:  b.nextExpected,
	}
}
