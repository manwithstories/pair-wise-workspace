// Package ingest validates RTP packets and fMP4/MP4 boxes coming from the
// capture nodes. It performs no network IO: everything arrives as byte
// slices produced by local fixtures, and every output is a validated
// structure ready for the timeline stage.
package ingest

import (
	"fmt"

	"github.com/clearliu/fieldcut/internal/clock"
	"github.com/pion/rtp"
)

// MaxPacketBytes is the hard ceiling on an accepted RTP packet (jumbo
// frame sized); larger datagrams are rejected rather than buffered.
const MaxPacketBytes = 9000

// Source records how a packet entered the reorder buffer, so downstream
// stages can attribute recovered data instead of silently trusting it.
type Source int

const (
	// SourceLive is a packet arriving on the primary stream, in order.
	SourceLive Source = iota
	// SourceRTX is a retransmission filling a previously seen hole.
	SourceRTX
	// SourceLate is a primary-stream packet arriving after its position
	// had already been released (the gap marker stays in the stream).
	SourceLate
)

func (s Source) String() string {
	switch s {
	case SourceRTX:
		return "rtx"
	case SourceLate:
		return "late"
	default:
		return "live"
	}
}

// ReorderConfig parameterizes the sequence window. Zero values select
// safe defaults; every field is hard-capped.
type ReorderConfig struct {
	// WindowSize is the maximum out-of-order distance tolerated before
	// missing packets are declared unrecoverable gaps.
	WindowSize uint16
	// MaxBuffered caps held packets independent of the sequence window.
	MaxBuffered int
}

func (c ReorderConfig) withDefaults() ReorderConfig {
	if c.WindowSize == 0 {
		c.WindowSize = 64
	}
	if c.WindowSize > 4096 {
		c.WindowSize = 4096
	}
	if c.MaxBuffered <= 0 {
		c.MaxBuffered = int(c.WindowSize) * 2
	}
	if c.MaxBuffered > 65535 {
		c.MaxBuffered = 65535
	}
	return c
}

// OrderedPacket is one element of the deterministic output stream. When
// IsGap is true, Packet is nil and the entry records an unrecoverable
// sequence hole; fieldcut never fabricates a frame to fill it.
type OrderedPacket struct {
	Packet    *rtp.Packet
	Source    Source
	Recovered bool
	IsGap     bool
	GapStart  uint16
	GapCount  int
}

// ReorderStats accumulates pipeline-visible counters. All counters are
// deterministic for a fixed input sequence.
type ReorderStats struct {
	Received       int
	Duplicates     int
	Released       int
	RecoveredRTX   int
	RecoveredLate  int
	Gaps           int
	GapPackets     int
	ExpiredLate    int
	PeakBuffered   int
}

// ReorderBuffer reorders RTP packets by 16-bit sequence number, recovers
// holes via RTX or late arrivals and preserves unrecoverable gaps. The
// buffer is bounded by both sequence distance and packet count.
type ReorderBuffer struct {
	cfg     ReorderConfig
	started bool
	next    uint64 // extended sequence number expected next
	last    uint64 // highest extended sequence ever observed
	buf     map[uint64]*rtp.Packet
	sources map[uint64]Source
	seen    map[uint64]bool
	stats   ReorderStats
}

// NewReorderBuffer constructs a buffer with the given configuration.
func NewReorderBuffer(cfg ReorderConfig) *ReorderBuffer {
	cfg = cfg.withDefaults()
	return &ReorderBuffer{
		cfg:     cfg,
		buf:     map[uint64]*rtp.Packet{},
		sources: map[uint64]Source{},
		seen:    map[uint64]bool{},
	}
}

// Stats returns a copy of the running counters.
func (b *ReorderBuffer) Stats() ReorderStats { return b.stats }

// extend maps a 16-bit wire sequence number onto the 64-bit axis, using
// the distance to the most recently observed packet to pick the wrap.
func (b *ReorderBuffer) extend(seq uint16) uint64 {
	if !b.started {
		return uint64(seq)
	}
	ref := b.last + 1
	base := ref - ref%65536
	cand := base + uint64(seq)
	d := int64(cand) - int64(ref)
	if d > 32767 {
		cand -= 65536
	} else if d < -32768 {
		cand += 65536
	}
	return cand
}

// Push validates and ingests one packet. Ordered output (including gap
// markers forced by an over-window jump) is returned in release order;
// late packets that can no longer be inserted are returned separately so
// callers can feed them to the replay engine without rewriting history.
func (b *ReorderBuffer) Push(pkt *rtp.Packet, source Source) (ordered []OrderedPacket, late []OrderedPacket, err error) {
	if pkt == nil {
		return nil, nil, fmt.Errorf("%w: nil packet", clock.ErrInvalidArgument)
	}
	if err := ValidatePacket(pkt); err != nil {
		return nil, nil, err
	}
	b.stats.Received++
	ext := b.extend(pkt.SequenceNumber)
	if !b.started {
		b.started = true
		b.next, b.last = ext, ext
	}

	// Already-released positions are handled first: a re-arrival after
	// release is a late fill (replay candidate), not a mere duplicate.
	if ext < b.next {
		dist := b.next - ext
		if dist > uint64(b.cfg.WindowSize) {
			b.stats.ExpiredLate++
			return nil, []OrderedPacket{{Packet: pkt, Source: SourceLate, Recovered: false}}, nil
		}
		b.stats.RecoveredLate++
		return nil, []OrderedPacket{{Packet: pkt, Source: SourceLate, Recovered: true}}, nil
	}
	if b.seen[ext] {
		b.stats.Duplicates++
		return nil, nil, nil
	}

	// Forward packet beyond the window first forces gap finalization.
	if ext > b.next+uint64(b.cfg.WindowSize) {
		ordered = append(ordered, b.finalizeGap(ext)...)
	}
	if len(b.buf) >= b.cfg.MaxBuffered && b.buf[ext] == nil {
		return nil, nil, fmt.Errorf("%w: reorder buffer capacity %d exceeded", clock.ErrQueueFull, b.cfg.MaxBuffered)
	}
	b.buf[ext] = pkt
	b.sources[ext] = source
	b.seen[ext] = true
	if ext > b.last {
		b.last = ext
	}
	if len(b.buf) > b.stats.PeakBuffered {
		b.stats.PeakBuffered = len(b.buf)
	}
	for {
		p, ok := b.buf[b.next]
		if !ok {
			break
		}
		src := b.sources[b.next]
		recovered := src != SourceLive
		if recovered {
			b.stats.RecoveredRTX++
		}
		ordered = append(ordered, OrderedPacket{Packet: p, Source: src, Recovered: recovered})
		delete(b.buf, b.next)
		delete(b.sources, b.next)
		b.next++
		b.stats.Released++
	}
	return ordered, nil, nil
}

// finalizeGap emits one gap marker covering [next, target) and advances
// the expectation to target.
func (b *ReorderBuffer) finalizeGap(target uint64) []OrderedPacket {
	count := int(target - b.next)
	gap := OrderedPacket{
		IsGap:    true,
		GapStart: uint16(b.next),
		GapCount: count,
	}
	b.stats.Gaps++
	b.stats.GapPackets += count
	b.next = target
	return []OrderedPacket{gap}
}

// Flush emits gap markers for everything still missing and releases all
// buffered packets in sequence order. It is called at stream end so no
// hole is silently dropped.
func (b *ReorderBuffer) Flush() []OrderedPacket {
	if !b.started {
		return nil
	}
	var out []OrderedPacket
	for len(b.buf) > 0 {
		if p, ok := b.buf[b.next]; ok {
			src := b.sources[b.next]
			out = append(out, OrderedPacket{Packet: p, Source: src, Recovered: src != SourceLive})
			delete(b.buf, b.next)
			delete(b.sources, b.next)
			b.stats.Released++
			b.next++
		} else {
			// finalizeGap advances next by one itself.
			out = append(out, b.finalizeGap(b.next+1)...)
		}
	}
	return out
}

// ParsePacket unmarshals a datagram with a hard size cap.
func ParsePacket(raw []byte) (*rtp.Packet, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: empty datagram", clock.ErrInvalidArgument)
	}
	if len(raw) > MaxPacketBytes {
		return nil, fmt.Errorf("%w: packet %d bytes > %d", clock.ErrInvalidArgument, len(raw), MaxPacketBytes)
	}
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(raw); err != nil {
		return nil, fmt.Errorf("%w: %v", clock.ErrInvalidArgument, err)
	}
	return pkt, ValidatePacket(pkt)
}

// ValidatePacket enforces RTPv2 framing invariants that Unmarshal alone
// does not treat as fatal.
func ValidatePacket(pkt *rtp.Packet) error {
	if pkt == nil {
		return fmt.Errorf("%w: nil packet", clock.ErrInvalidArgument)
	}
	if pkt.Version != 2 {
		return fmt.Errorf("%w: RTP version %d != 2", clock.ErrInvalidArgument, pkt.Version)
	}
	if len(pkt.Payload) == 0 {
		return fmt.Errorf("%w: empty payload for seq %d", clock.ErrInvalidArgument, pkt.SequenceNumber)
	}
	if int(pkt.MarshalSize()) > MaxPacketBytes {
		return fmt.Errorf("%w: packet %d bytes > %d", clock.ErrInvalidArgument, pkt.MarshalSize(), MaxPacketBytes)
	}
	return nil
}
