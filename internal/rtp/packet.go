// Package rtp carries the receive path for the media track: packets are
// assembled with pion/rtp, put through a jitter/loss simulation, then
// reordered, deduplicated and reassembled against an extended sequence
// number. Recoverable gaps (within the window) and unrecoverable gaps (past
// it) are counted so recovery can be quantified.
package rtp

import (
	"fmt"
	"sort"

	"github.com/pion/rtp"

	"avpipe/internal/fault"
)

// Packet is one media packet with a timestamp on the 90 kHz grid.
type Packet struct {
	Seq       uint16
	Timestamp uint32
	SSRC      uint32
	Payload   []byte
	Marker    bool
}

// Marshal encodes the packet with pion/rtp.
func (p Packet) Marshal() ([]byte, error) {
	pkt := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         p.Marker,
			PayloadType:    111,
			SequenceNumber: p.Seq,
			Timestamp:      p.Timestamp,
			SSRC:           p.SSRC,
		},
		Payload: p.Payload,
	}
	buf, err := pkt.Marshal()
	if err != nil {
		return nil, fault.Wrap(fault.CodeRTP, "rtp.marshal", err, "seq %d", p.Seq)
	}
	return buf, nil
}

// Unmarshal decodes a wire packet with pion/rtp.
func Unmarshal(buf []byte) (Packet, error) {
	var pkt rtp.Packet
	if err := pkt.Unmarshal(buf); err != nil {
		return Packet{}, fault.Wrap(fault.CodeRTP, "rtp.unmarshal", err,
			"%d-byte packet", len(buf))
	}
	return Packet{
		Seq: pkt.SequenceNumber, Timestamp: pkt.Timestamp, SSRC: pkt.SSRC,
		Payload: append([]byte(nil), pkt.Payload...), Marker: pkt.Marker,
	}, nil
}

// Extend maps a 16-bit sequence number to a monotonically increasing 64-bit
// value using a reference point. The result is the nearest extension of seq
// to ref, which handles wraparound and large reordering windows.
func Extend(seq uint16, ref uint64) uint64 {
	base := ref &^ 0xFFFF
	cand := base | uint64(seq)
	switch {
	case cand+0x8000 < ref:
		cand += 0x10000
	case cand > ref+0x8000 && cand >= 0x10000:
		cand -= 0x10000
	}
	return cand
}

// String renders a packet for diagnostics without payload bytes.
func (p Packet) String() string {
	return fmt.Sprintf("seq=%d ts=%d ssrc=%d len=%d marker=%v",
		p.Seq, p.Timestamp, p.SSRC, len(p.Payload), p.Marker)
}

// sortPackets sorts by extended sequence.
func sortPackets(ps []extended) {
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].ext < ps[j].ext })
}

type extended struct {
	ext uint64
	pkt Packet
}
