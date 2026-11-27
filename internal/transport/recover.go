package transport

import (
	"github.com/pion/rtp"
)

// RecoveryStats counts repair outcomes. Counters are cumulative and
// idempotent under resume: a recovered packet is counted exactly once
// because recovery is keyed by sequence number.
type RecoveryStats struct {
	TotalPackets       int `json:"total_packets"`
	DeliveredPackets   int `json:"delivered_packets"`
	LostPackets        int `json:"lost_packets"`
	NACKRecovered      int `json:"recovered_via_nack"`
	XORRecovered       int `json:"recovered_via_xor"`
	UnrecoveredPackets int `json:"unrecovered_packets"`
	// RecoveredPackets is the headline figure required by the CLI contract.
	RecoveredPackets int `json:"recovered_packets"`
}

// nackCache retains recently seen packets so a NACK for a packet that was
// merely reordered (not truly lost) can be satisfied locally.
type nackCache struct {
	depth int
	ring  []*Packet
	pos   int
	full  bool
}

func newNACKCache(depth int) *nackCache {
	return &nackCache{depth: depth, ring: make([]*Packet, depth)}
}

func (c *nackCache) add(p *Packet) {
	c.ring[c.pos] = p
	c.pos = (c.pos + 1) % c.depth
	if c.pos == 0 {
		c.full = true
	}
}

func (c *nackCache) lookup(seq uint16) *Packet {
	n := c.depth
	if !c.full {
		n = c.pos
	}
	for i := 0; i < n; i++ {
		idx := (c.pos - 1 - i + c.depth) % c.depth
		if p := c.ring[idx]; p != nil && p.RTP.SequenceNumber == seq {
			return p
		}
	}
	return nil
}

// xorBlock is one single-packet XOR FEC block: parity = A xor B. If either
// member is lost and the other two (parity + surviving member) arrive, the
// missing packet is reconstructed exactly.
type xorBlock struct {
	seqA, seqB, seqParity uint16
}

// parityFor builds (or looks up) the FEC block assignment for a sequence.
// Blocks are formed on consecutive original packets; the parity packet's
// synthetic sequence is derived so it never collides with originals.
func blockFor(seq uint16) xorBlock {
	base := (seq / 2) * 2
	return xorBlock{seqA: base, seqB: base + 1, seqParity: base}
}

// xorPayload computes byte-wise XOR, zero-padding the shorter operand.
func xorPayload(a, b []byte) []byte {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		var x, y byte
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		out[i] = x ^ y
	}
	return out
}

// reconstruct builds a recovered Packet from a surviving packet and the
// parity payload, preserving the RTP header of the missing sequence.
func reconstruct(missingSeq uint16, survivor *Packet, parity []byte) *Packet {
	payload := xorPayload(survivor.RTP.Payload, parity)
	hdr := survivor.RTP.Header
	hdr.SequenceNumber = missingSeq
	return &Packet{
		RTP:        &rtp.Packet{Header: hdr, Payload: payload},
		SampleIdx:  survivor.SampleIdx,
		TrackIdx:   survivor.TrackIdx,
		PTS:        survivor.PTS,
		DTS:        survivor.DTS,
		IsKeyframe: survivor.IsKeyframe,
	}
}
