// Package rtp implements bounded RTP reordering, deduplication and recovery.
package rtp

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/pion/rtp"

	"medialab/internal/clock"
	"medialab/internal/model"
)

const (
	// DefaultReorderWindow is the fixture/jitter buffer reorder span.
	DefaultReorderWindow = 16
	// MaxReorderWindow hard-bounds pending out-of-order packets.
	MaxReorderWindow = 64
	maxSeenHistory   = 128
	maxQuarantine    = 32
	// MaxPacketBytes bounds one locally generated or parsed RTP packet.
	MaxPacketBytes = 64 << 10
)

// Disposition tells the pipeline how a pushed packet was handled.
type Disposition string

const (
	DispositionDelivered Disposition = "delivered"
	DispositionBuffered  Disposition = "buffered"
	DispositionDuplicate Disposition = "duplicate_dropped"
	DispositionIsolated  Disposition = "packet_isolated"
	DispositionStale     Disposition = "stale_dropped"
	DispositionOverflow  Disposition = "overflow_dropped"
)

// InPacket retains provenance beside the pion RTP packet.
type InPacket struct {
	P         rtp.Packet
	TrackID   uint32
	RawOffset int64
	Reordered bool
}

// Conflict is an isolated same-sequence packet with a different payload.
type Conflict struct {
	Sequence     uint16
	FirstOffset  int64
	SecondOffset int64
	FirstHash    string
	SecondHash   string
	Fault        *model.Fault
}

// Stats are deterministic counters used by reports and tests.
type Stats struct {
	Pushed            int
	DuplicateDropped  int
	ConflictsIsolated int
	ReorderedPackets  int
	DeliveredPackets  int
	StaleDropped      int
	OverflowDropped   int
}

// PushResult contains zero or more in-order packets and handling provenance.
type PushResult struct {
	Ready       []*InPacket
	Disposition Disposition
	Fault       *model.Fault
}

type seenInfo struct {
	hash   [32]byte
	offset int64
}

// ReorderBuffer is a bounded sequence-number reorder and dedupe buffer.
type ReorderBuffer struct {
	window      int
	next        uint16
	initialized bool
	pending     map[uint16]*InPacket
	seen        map[uint16]seenInfo
	seenOrder   []uint16
	Quarantine  []Conflict
	Stats       Stats
}

// NewReorderBuffer creates a buffer with the requested forward window.
func NewReorderBuffer(window int) *ReorderBuffer {
	if window <= 0 {
		window = DefaultReorderWindow
	}
	if window > MaxReorderWindow {
		window = MaxReorderWindow
	}
	return &ReorderBuffer{
		window:  window,
		pending: make(map[uint16]*InPacket),
		seen:    make(map[uint16]seenInfo),
	}
}

// ParsePacket unmarshals one raw RTP packet and attaches its stream offset.
func ParsePacket(raw []byte, rawOffset int64, trackID uint32) (*InPacket, error) {
	if len(raw) == 0 || len(raw) > MaxPacketBytes {
		return nil, model.New(model.StageRTP, "packet_length_invalid", trackID, rawOffset, errors.New("RTP packet length outside bounded range"))
	}
	var p rtp.Packet
	if err := p.Unmarshal(raw); err != nil {
		return nil, model.New(model.StageRTP, "rtp_unmarshal", trackID, rawOffset, err)
	}
	return &InPacket{P: p, TrackID: trackID, RawOffset: rawOffset}, nil
}

// Push accepts one packet and returns any newly contiguous packets.
func (b *ReorderBuffer) Push(pkt *InPacket) PushResult {
	b.Stats.Pushed++
	seq := pkt.P.SequenceNumber
	sum := sha256.Sum256(pkt.P.Payload)

	if prior, ok := b.seen[seq]; ok {
		if prior.hash != sum {
			return b.isolateConflict(pkt, prior, sum)
		}
		b.Stats.DuplicateDropped++
		return PushResult{Disposition: DispositionDuplicate}
	}

	if !b.initialized {
		b.initialized = true
		b.next = seq + 1
		b.remember(seq, sum, pkt.RawOffset)
		b.Stats.DeliveredPackets++
		return PushResult{Ready: []*InPacket{pkt}, Disposition: DispositionDelivered}
	}

	delta := clock.DeltaUint16(b.next, seq)
	if delta == 0 {
		b.remember(seq, sum, pkt.RawOffset)
		ready := []*InPacket{pkt}
		b.Stats.DeliveredPackets++
		b.next++
		ready = append(ready, b.drainPending()...)
		return PushResult{Ready: ready, Disposition: DispositionDelivered}
	}
	if delta < 0 {
		b.Stats.StaleDropped++
		return PushResult{Disposition: DispositionStale}
	}
	if delta > b.window || len(b.pending) >= b.window {
		b.Stats.OverflowDropped++
		f := model.New(model.StageRTP, "reorder_window_overflow", pkt.TrackID, pkt.RawOffset,
			fmt.Errorf("sequence %d is %d ahead of expected %d", seq, delta, b.next))
		return PushResult{Disposition: DispositionOverflow, Fault: f}
	}

	pkt.Reordered = true
	b.pending[seq] = pkt
	b.remember(seq, sum, pkt.RawOffset)
	return PushResult{Disposition: DispositionBuffered}
}

func (b *ReorderBuffer) drainPending() []*InPacket {
	var ready []*InPacket
	for {
		pkt, ok := b.pending[b.next]
		if !ok {
			return ready
		}
		delete(b.pending, b.next)
		ready = append(ready, pkt)
		b.Stats.DeliveredPackets++
		if pkt.Reordered {
			b.Stats.ReorderedPackets++
		}
		b.next++ // uint16 arithmetic intentionally wraps at 65535 -> 0.
	}
}

func (b *ReorderBuffer) remember(seq uint16, sum [32]byte, offset int64) {
	if _, exists := b.seen[seq]; !exists {
		b.seenOrder = append(b.seenOrder, seq)
	}
	b.seen[seq] = seenInfo{hash: sum, offset: offset}
	if len(b.seenOrder) > maxSeenHistory {
		old := b.seenOrder[0]
		copy(b.seenOrder, b.seenOrder[1:])
		b.seenOrder = b.seenOrder[:len(b.seenOrder)-1]
		if _, pending := b.pending[old]; !pending {
			delete(b.seen, old)
		}
	}
}

func (b *ReorderBuffer) isolateConflict(pkt *InPacket, prior seenInfo, sum [32]byte) PushResult {
	f := model.New(model.StageRTP, "conflicting_sequence_payload", pkt.TrackID, pkt.RawOffset,
		fmt.Errorf("sequence %d already seen at offset %d with different payload", pkt.P.SequenceNumber, prior.offset))
	b.Stats.ConflictsIsolated++
	if len(b.Quarantine) < maxQuarantine {
		b.Quarantine = append(b.Quarantine, Conflict{
			Sequence:     pkt.P.SequenceNumber,
			FirstOffset:  prior.offset,
			SecondOffset: pkt.RawOffset,
			FirstHash:    fmt.Sprintf("%x", prior.hash),
			SecondHash:   fmt.Sprintf("%x", sum),
			Fault:        f,
		})
	}
	return PushResult{Disposition: DispositionIsolated, Fault: f}
}

// TimestampUnwrapper converts wrapping 32-bit RTP timestamps to a monotonic
// signed timeline using shortest-wrap deltas.
type TimestampUnwrapper struct {
	initialized bool
	last        uint32
	unwrapped   int64
}

// Unwrap returns the monotonic timestamp corresponding to ts.
func (u *TimestampUnwrapper) Unwrap(ts uint32) int64 {
	if !u.initialized {
		u.initialized = true
		u.last = ts
		u.unwrapped = int64(ts)
		return u.unwrapped
	}
	u.unwrapped += clock.DeltaUint32(u.last, ts)
	u.last = ts
	return u.unwrapped
}
