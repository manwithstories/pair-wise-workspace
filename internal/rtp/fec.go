package rtp

import (
	"github.com/pion/rtp"

	"avpipe/internal/media"
)

// Payload types distinguish media packets from XOR parity packets.
const (
	PayloadTypeMedia  uint8 = 96
	PayloadTypeParity uint8 = 97
	// FECGroupSize is the number of media packets covered by one parity.
	FECGroupSize = 4
	// SSRC identifies the single demo synchronization source.
	SSRC uint32 = 0xA710_1010
)

// Packetize wraps frame payloads into RTP media packets on the 90 kHz clock.
// Sequence numbers and timestamps come from the frame's sequence/PTS so the
// output is fully deterministic for a given seed/input.
func Packetize(frames []media.Frame, ptsStep uint64) []*rtp.Packet {
	pkts := make([]*rtp.Packet, 0, len(frames))
	for _, f := range frames {
		if f.Corrupt {
			continue // gaps never enter the RTP stream
		}
		pkts = append(pkts, &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    PayloadTypeMedia,
				SequenceNumber: uint16(f.Sequence),
				Timestamp:      uint32(f.PTS90k),
				SSRC:           SSRC,
				Marker:         true,
			},
			Payload: append([]byte(nil), f.Payload...),
		})
	}
	_ = ptsStep
	return pkts
}

// EncodeFEC appends one XOR parity packet per FECGroupSize media packets.
// Parity packets use PayloadTypeParity and carry the base sequence number in
// their low 16 bits of timestamp-independent metadata via SSRC grouping; the
// group base is encoded in the first 2 payload bytes.
func EncodeFEC(mediaPkts []*rtp.Packet) []*rtp.Packet {
	out := make([]*rtp.Packet, 0, len(mediaPkts)+len(mediaPkts)/FECGroupSize+1)
	var seq uint16 = 0xF000 // parity sequence space, distinct from media
	for i := 0; i < len(mediaPkts); i += FECGroupSize {
		end := i + FECGroupSize
		if end > len(mediaPkts) {
			end = len(mediaPkts)
		}
		group := mediaPkts[i:end]
		parity, n := xorPayloads(group)
		base := group[0].SequenceNumber
		hdr := make([]byte, 3)
		putU16(hdr[0:2], base)
		hdr[2] = byte(n)
		out = append(out, group...)
		out = append(out, &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    PayloadTypeParity,
				SequenceNumber: seq,
				Timestamp:      group[0].Timestamp,
				SSRC:           SSRC,
				Marker:         false,
			},
			Payload: append(hdr, parity...),
		})
		seq++
	}
	return out
}

// xorPayloads XORs up to FECGroupSize payloads, zero-padding shorter members
// to the longest length.
func xorPayloads(group []*rtp.Packet) ([]byte, int) {
	maxLen := 0
	for _, p := range group {
		if len(p.Payload) > maxLen {
			maxLen = len(p.Payload)
		}
	}
	out := make([]byte, maxLen)
	for _, p := range group {
		for i, b := range p.Payload {
			out[i] ^= b
		}
	}
	return out, maxLen
}

// FECGroup bundles a received parity packet with the media packets sharing
// its base sequence.
type FECGroup struct {
	Base    uint16
	Size    int
	Parity  *rtp.Packet
	Present map[uint16]*rtp.Packet
}

// DecodeFEC attempts to reconstruct a single missing media packet in every
// group that has exactly one loss. It returns the rebuilt packets keyed by
// sequence number and the number of groups that could not be repaired.
func DecodeFEC(groups []FECGroup) (recovered map[uint16]*rtp.Packet, unrecoverable int) {
	recovered = make(map[uint16]*rtp.Packet)
	for _, g := range groups {
		missing := g.Size - len(g.Present)
		if missing <= 0 {
			continue
		}
		if missing > 1 || g.Parity == nil {
			unrecoverable++
			continue
		}
		var missingSeq uint16
		for k := 0; k < g.Size; k++ {
			seq := g.Base + uint16(k)
			if _, ok := g.Present[seq]; !ok {
				missingSeq = seq
				break
			}
		}
		payloadLen := int(g.Parity.Payload[2])
		body := g.Parity.Payload[3:]
		rebuilt := make([]byte, payloadLen)
		copy(rebuilt, body[:min(len(body), payloadLen)])
		for seq, p := range g.Present {
			for i := 0; i < payloadLen && i < len(p.Payload); i++ {
				rebuilt[i] ^= p.Payload[i]
			}
			_ = seq
		}
		recovered[missingSeq] = &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    PayloadTypeMedia,
				SequenceNumber: missingSeq,
				Timestamp:      g.Parity.Timestamp,
				SSRC:           SSRC,
				Marker:         true,
			},
			Payload: rebuilt,
		}
	}
	return recovered, unrecoverable
}

// ParseParity extracts the base sequence and group size from a parity packet.
func ParseParity(p *rtp.Packet) (base uint16, size int, ok bool) {
	if p.PayloadType != PayloadTypeParity || len(p.Payload) < 3 {
		return 0, 0, false
	}
	return u16(p.Payload[0:2]), int(p.Payload[2]), true
}

func putU16(b []byte, v uint16) { b[0] = byte(v >> 8); b[1] = byte(v) }
func u16(b []byte) uint16       { return uint16(b[0])<<8 | uint16(b[1]) }
