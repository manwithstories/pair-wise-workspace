// Package rtp wraps media frames into RTP packets, simulates a lossy
// mobile-network path, reorders them through a bounded jitter buffer and
// reconstructs losses with packet-level XOR FEC.
package rtp

import (
	"math/rand"

	"github.com/pion/rtp"

	"avpipe/internal/media"
)

// NetworkConfig parameterizes the deterministic path simulation.
type NetworkConfig struct {
	// LossRate is the independent per-packet drop probability (0..1).
	LossRate float64
	// Burst is the additional run length applied when a loss starts, so a
	// loss event drops 1+Burst consecutive packets.
	Burst int
	// JitterTicks shifts each packet's arrival position by up to this many
	// slots on the 90 kHz timeline.
	JitterTicks uint64
	// Seed makes every run reproducible.
	Seed int64
}

// TimedPacket pairs an RTP packet with its simulated arrival timestamp
// (90 kHz ticks).
type TimedPacket struct {
	Packet    *rtp.Packet
	Arrive90k uint64
	Lost      bool
}

// SimulateNetwork applies deterministic loss (with bursts) and jitter to an
// ordered packet slice. Lost packets keep their slot with Lost=true so the
// receiver can reason about expected sequence numbers.
func SimulateNetwork(pkts []*rtp.Packet, cfg NetworkConfig) []TimedPacket {
	rng := rand.New(rand.NewSource(cfg.Seed))
	out := make([]TimedPacket, len(pkts))
	burstLeft := 0
	for i, p := range pkts {
		lost := false
		if burstLeft > 0 {
			lost = true
			burstLeft--
		} else if cfg.LossRate > 0 && rng.Float64() < cfg.LossRate {
			lost = true
			burstLeft = cfg.Burst
		}
		// Jitter draws only when enabled, so the loss RNG sequence stays
		// independent of whether jitter simulation is turned on.
		var jitter uint64
		if cfg.JitterTicks > 0 {
			jitter = uint64(rng.Int63n(int64(cfg.JitterTicks + 1)))
		}
		out[i] = TimedPacket{
			Packet:    p,
			Arrive90k: uint64(p.Timestamp) + jitter,
			Lost:      lost,
		}
	}
	stableSortByArrival(out)
	return out
}

// stableSortByArrival is a small insertion sort; packet counts are bounded by
// the pipeline's queue limits so the quadratic cost is acceptable and it
// stays deterministic without extra allocations.
func stableSortByArrival(p []TimedPacket) {
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j-1].Arrive90k > p[j].Arrive90k; j-- {
			p[j-1], p[j] = p[j], p[j-1]
		}
	}
}

// JitterBuffer reorders packets by RTP sequence number within a bounded wait
// window. Each missing sequence gets an independent expiry clock; when it
// elapses the sequence is emitted as a gap and the buffer advances instead of
// blocking the pipeline.
type JitterBuffer struct {
	capacity  int
	window90k uint64
	buf       map[uint16]*rtp.Packet
	nextSeq   uint16
	// haveNext is false until the first packet establishes the cursor.
	haveNext bool
	// playing becomes true once Drain has emitted the first packet; before
	// then out-of-order arrivals may still lower the cursor.
	playing   bool
	expireAt  uint64
	waiting   bool
	lateDrops int
	overflows int
}

// NewJitterBuffer builds a buffer with hard capacity (queue limit) and a wait
// window expressed in 90 kHz ticks.
func NewJitterBuffer(capacity int, window90k uint64) *JitterBuffer {
	return &JitterBuffer{
		capacity:  capacity,
		window90k: window90k,
		buf:       make(map[uint16]*rtp.Packet),
	}
}

// Push stores one received packet. A full buffer returns a locatable
// limit-exceeded error so callers can surface cache pressure.
func (b *JitterBuffer) Push(p *rtp.Packet) error {
	if _, exists := b.buf[p.SequenceNumber]; exists {
		return nil // duplicate, ignore idempotently
	}
	if !b.haveNext {
		b.nextSeq = p.SequenceNumber
		b.haveNext = true
	} else if !b.playing && seqLess(p.SequenceNumber, b.nextSeq) {
		// Reordering arrived before playout: adopt the lower sequence as the
		// true start so earlier packets are not mistaken for late ones.
		b.nextSeq = p.SequenceNumber
	} else if b.playing && seqLess(p.SequenceNumber, b.nextSeq) {
		// Playout has passed this sequence; the packet can never be used.
		b.lateDrops++
		return nil
	}
	if len(b.buf) >= b.capacity {
		b.overflows++
		return media.NewError("rtp", media.KindLimitExceeded, 0,
			media.ActionLimit, "jitter buffer capacity %d reached", b.capacity)
	}
	b.buf[p.SequenceNumber] = p
	return nil
}

// seqLess reports whether sequence a precedes b, handling uint16 wrap-around
// by interpreting the modular difference as signed.
func seqLess(a, b uint16) bool {
	return int16(a-b) < 0
}

// Drain emits every in-order packet currently available. It only starts a
// wait window when the next expected sequence is genuinely missing. If the
// window has elapsed the missing sequence is emitted as an explicit gap and
// Drain continues, so the call always terminates.
func (b *JitterBuffer) Drain(now90k uint64) []Playout {
	var out []Playout
	for {
		// Fast path: emit every contiguous packet already in the buffer.
		if p, ok := b.buf[b.nextSeq]; ok {
			b.playing = true
			out = append(out, Playout{Packet: p, Seq: b.nextSeq})
			delete(b.buf, b.nextSeq)
			b.nextSeq++
			b.waiting = false
			continue
		}
		// Slow path: the next sequence is missing. Arm or check its window.
		if !b.waiting {
			b.waiting = true
			b.expireAt = now90k + b.window90k
		}
		if now90k < b.expireAt {
			break // still inside the wait window for this sequence
		}
		b.playing = true
		out = append(out, Playout{Seq: b.nextSeq, Gap: true})
		b.nextSeq++
		// Re-arm for the next possibly missing sequence.
		b.waiting = false
	}
	return out
}

// Flush emits every remaining buffered packet in sequence order and, given
// the total expected sequence count, fills every still-missing sequence with
// an explicit gap. It ignores wait windows and is used for end-of-stream
// settlement so no late gap is silently swallowed.
func (b *JitterBuffer) Flush(expectedCount uint16) []Playout {
	if !b.haveNext || expectedCount == 0 {
		return nil
	}
	var out []Playout
	// Drain buffered packets first so their order is preserved.
	for len(b.buf) > 0 {
		if p, ok := b.buf[b.nextSeq]; ok {
			b.playing = true
			out = append(out, Playout{Packet: p, Seq: b.nextSeq})
			delete(b.buf, b.nextSeq)
			b.nextSeq++
			continue
		}
		out = append(out, Playout{Seq: b.nextSeq, Gap: true})
		b.playing = true
		b.nextSeq++
	}
	return out
}

// Stats reports buffer pressure counters.
func (b *JitterBuffer) Stats() (lateDrops, overflows int) {
	return b.lateDrops, b.overflows
}

// Playout is one jitter-buffer output: either a packet or an explicit gap.
type Playout struct {
	Packet *rtp.Packet
	Seq    uint16
	Gap    bool
}
