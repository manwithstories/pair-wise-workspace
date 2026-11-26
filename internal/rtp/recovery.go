package rtp

import (
	"fmt"

	"medialab/internal/model"
)

const (
	// MaxNACKEntries bounds a single simulated retransmission request.
	MaxNACKEntries = 32
	// MaxRetransmitCache bounds cached sent packets.
	MaxRetransmitCache = 128
	// MaxXORGroup bounds packets protected by one parity equation.
	MaxXORGroup = 16
)

// CachedPacket is a sent packet plus the local time it entered the cache.
type CachedPacket struct {
	Pkt      *InPacket
	StoredAt int64
}

// RetransmitCache is a bounded FIFO of recently sent packets. Gets after TTL
// miss, modeling the limited time in which retransmission can meet playout.
type RetransmitCache struct {
	ttlMS int64
	max   int
	items map[uint16]CachedPacket
	order []uint16
}

// NewRetransmitCache creates a bounded cache.
func NewRetransmitCache(ttlMS int64, max int) *RetransmitCache {
	if max <= 0 || max > MaxRetransmitCache {
		max = MaxRetransmitCache
	}
	if ttlMS <= 0 {
		ttlMS = 80
	}
	return &RetransmitCache{ttlMS: ttlMS, max: max, items: make(map[uint16]CachedPacket)}
}

// Store retains a sent packet. Re-storing a sequence refreshes it.
func (c *RetransmitCache) Store(nowMS int64, pkt *InPacket) {
	seq := pkt.P.SequenceNumber
	if _, exists := c.items[seq]; !exists {
		c.order = append(c.order, seq)
	}
	c.items[seq] = CachedPacket{Pkt: pkt, StoredAt: nowMS}
	for len(c.order) > c.max {
		old := c.order[0]
		c.order = c.order[1:]
		delete(c.items, old)
	}
}

// Get returns the packet only if it is present and within the TTL.
func (c *RetransmitCache) Get(seq uint16, nowMS int64) (*InPacket, bool) {
	item, ok := c.items[seq]
	if !ok {
		return nil, false
	}
	if nowMS-item.StoredAt > c.ttlMS {
		delete(c.items, seq)
		return nil, false
	}
	return item.Pkt, true
}

// NewNACK validates and stores a bounded sequence request.
func NewNACK(seqs []uint16, requestedAtMS int64) (*NACK, error) {
	if len(seqs) > MaxNACKEntries {
		return nil, model.New(model.StageRecovery, "nack_too_large", 0, -1,
			fmt.Errorf("NACK contains %d entries, limit %d", len(seqs), MaxNACKEntries))
	}
	return &NACK{Sequences: append([]uint16(nil), seqs...), RequestedAtMS: requestedAtMS}, nil
}

// NACK is a simulated negative acknowledgement.
type NACK struct {
	Sequences     []uint16
	RequestedAtMS int64
}

// XORPayloads returns XOR of all payloads, treating missing trailing bytes as
// zero. All fixture segments in a group use equal-length payloads.
func XORPayloads(payloads ...[]byte) []byte {
	n := 0
	for _, p := range payloads {
		if len(p) > n {
			n = len(p)
		}
	}
	out := make([]byte, n)
	for _, p := range payloads {
		for i, b := range p {
			out[i] ^= b
		}
	}
	return out
}

// RecoverWithXOR combines parity with every available group member.
func RecoverWithXOR(parity []byte, available ...[]byte) []byte {
	return XORPayloads(append([][]byte{parity}, available...)...)
}

// XORMember preserves sequence and original stream offset for repair.
type XORMember struct {
	Sequence  uint16
	Timestamp uint32
	Offset    int64
	TrackID   uint32
}

// XORGroup is one parity equation. Recovery is possible when exactly one
// member payload is absent.
type XORGroup struct {
	ID      string
	Members []XORMember
	Parity  []byte
}

// BuildXORGroup creates one bounded XOR repair group.
func BuildXORGroup(id string, pkts ...*InPacket) (XORGroup, error) {
	if len(pkts) < 2 || len(pkts) > MaxXORGroup {
		return XORGroup{}, model.New(model.StageRecovery, "xor_group_size_invalid", 0, -1,
			fmt.Errorf("XOR group %q has %d packets, allowed 2..%d", id, len(pkts), MaxXORGroup))
	}
	payloads := make([][]byte, 0, len(pkts))
	members := make([]XORMember, 0, len(pkts))
	for _, p := range pkts {
		payloads = append(payloads, p.P.Payload)
		members = append(members, XORMember{
			Sequence: p.P.SequenceNumber, Timestamp: p.P.Timestamp,
			Offset: p.RawOffset, TrackID: p.TrackID,
		})
	}
	return XORGroup{ID: id, Members: members, Parity: XORPayloads(payloads...)}, nil
}

// Recover returns the sole missing member when all other members are present.
func (g XORGroup) Recover(available map[uint16]*InPacket) (*InPacket, bool) {
	var missing XORMember
	missingCount := 0
	var payloads [][]byte
	var trackID uint32
	for _, m := range g.Members {
		if p, ok := available[m.Sequence]; ok {
			payloads = append(payloads, p.P.Payload)
			trackID = p.TrackID
		} else {
			missing = m
			missingCount++
		}
	}
	if missingCount != 1 {
		return nil, false
	}
	recovered := RecoverWithXOR(g.Parity, payloads...)
	return &InPacket{
		P:         rtpPacketWithTimestamp(missing.Sequence, missing.Timestamp, recovered),
		TrackID:   firstNonZero(trackID, missing.TrackID),
		RawOffset: missing.Offset,
	}, true
}

func firstNonZero(a, b uint32) uint32 {
	if a != 0 {
		return a
	}
	return b
}
