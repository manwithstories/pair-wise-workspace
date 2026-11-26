package transport

import (
	"fmt"

	"github.com/pion/rtp"
)

// RTP constants used by the generated local samples.
const (
	VideoPayloadType uint8  = 96
	AudioPayloadType uint8  = 97
	DefaultSSRC      uint32 = 0x50414952 // "PAIR"
	DefaultMTU       int    = 1200
)

// ObservedPacket is an RTP packet plus deterministic local arrival metadata.
type ObservedPacket struct {
	Packet     rtp.Packet
	ArrivalNS  int64
	DeadlineNS int64
}

// Sequence returns the 16-bit RTP sequence number.
func (p ObservedPacket) Sequence() uint16 { return p.Packet.SequenceNumber }

// BuildPacket constructs one version-2 RTP packet.
func BuildPacket(seq uint16, ts uint32, ssrc uint32, payloadType uint8, payload []byte, marker bool) rtp.Packet {
	return rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Padding:        false,
			Extension:      false,
			Marker:         marker,
			PayloadType:    payloadType,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
		},
		Payload: payload,
	}
}

// MarshalPacket encodes through pion/rtp so the sample exercises the real wire
// representation rather than an in-memory-only struct.
func MarshalPacket(p rtp.Packet) ([]byte, error) {
	buf, err := p.Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshal RTP seq=%d: %w", p.SequenceNumber, err)
	}
	return buf, nil
}

// ParsePacket decodes a wire packet with pion/rtp.
func ParsePacket(b []byte) (rtp.Packet, error) {
	var p rtp.Packet
	if err := p.Unmarshal(b); err != nil {
		return rtp.Packet{}, fmt.Errorf("parse RTP packet: %w", err)
	}
	return p, nil
}

// PacketizeFrame splits one frame payload into MTU-sized RTP payloads. The
// marker bit is set on the final fragment. Sequence numbers are the caller's
// responsibility and are incremented across fragments.
func PacketizeFrame(seqStart uint16, ts uint32, ssrc uint32, payload []byte, mtu int) []rtp.Packet {
	if mtu <= 0 {
		mtu = DefaultMTU
	}
	if len(payload) == 0 {
		return []rtp.Packet{BuildPacket(seqStart, ts, ssrc, VideoPayloadType, nil, true)}
	}
	var out []rtp.Packet
	seq := seqStart
	for off := 0; off < len(payload); off += mtu {
		end := min(off+mtu, len(payload))
		last := end == len(payload)
		out = append(out, BuildPacket(seq, ts, ssrc, VideoPayloadType, append([]byte(nil), payload[off:end]...), last))
		seq++
	}
	return out
}

// RoundTrip encodes and decodes a packet, returning the decoded form. It is
// used by tests and the proof's RTP sample self-check.
func RoundTrip(p rtp.Packet) (rtp.Packet, []byte, error) {
	raw, err := MarshalPacket(p)
	if err != nil {
		return rtp.Packet{}, nil, err
	}
	decoded, err := ParsePacket(raw)
	if err != nil {
		return rtp.Packet{}, raw, err
	}
	return decoded, raw, nil
}
