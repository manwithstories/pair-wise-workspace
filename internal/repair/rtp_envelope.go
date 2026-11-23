package repair

import (
	"encoding/binary"

	"github.com/pion/rtp"
)

// Envelope layout inside an RTP payload:
//
//	flags        uint8   (bit0: a redundant copy follows)
//	redSeq       uint16  (sequence number the copy belongs to)
//	redLen       uint16
//	redundant    [redLen]
//	primary      remaining bytes
//
// The redundant block carries the primary payload of the previous sequence
// number, which makes isolated losses recoverable without retransmission.
const redHeaderLen = 5

// Packet is a decoded RTP media packet.
type Packet struct {
	Seq       uint16
	Timestamp uint32
	SSRC      uint32
	Marker    bool
	Payload   []byte
	Redundant *Redundant
	RawSize   int
}

// Redundant is the copy of a preceding packet's payload.
type Redundant struct {
	Seq   uint16
	Bytes []byte
}

// EncodePayload builds the envelope for a primary payload and an optional
// redundant copy of a previous packet.
func EncodePayload(primary []byte, red *Redundant) []byte {
	if red == nil {
		out := make([]byte, 1+len(primary))
		copy(out[1:], primary)
		return out
	}
	out := make([]byte, 1+redHeaderLen-1+len(red.Bytes)+len(primary))
	out[0] = 1
	binary.BigEndian.PutUint16(out[1:3], red.Seq)
	binary.BigEndian.PutUint16(out[3:5], uint16(len(red.Bytes)))
	copy(out[5:], red.Bytes)
	copy(out[5+len(red.Bytes):], primary)
	return out
}

// DecodeEnvelope splits an RTP payload into its primary and redundant parts.
func DecodeEnvelope(payload []byte) ([]byte, *Redundant, error) {
	if len(payload) < 1 {
		return nil, nil, ErrShortPayload
	}
	flags := payload[0]
	rest := payload[1:]
	if flags&1 == 0 {
		return rest, nil, nil
	}
	if len(rest) < redHeaderLen-1 {
		return nil, nil, ErrBadRedundancy
	}
	seq := binary.BigEndian.Uint16(rest[0:2])
	n := int(binary.BigEndian.Uint16(rest[2:4]))
	if n > len(rest)-4 {
		return nil, nil, ErrBadRedundancy
	}
	red := &Redundant{Seq: seq, Bytes: rest[4 : 4+n]}
	return rest[4+n:], red, nil
}

// Decode parses a raw RTP datagram.
func Decode(raw []byte) (*Packet, error) {
	var p rtp.Packet
	if err := p.Unmarshal(raw); err != nil {
		return nil, err
	}
	primary, red, err := DecodeEnvelope(p.Payload)
	if err != nil {
		return nil, err
	}
	return &Packet{
		Seq:       p.SequenceNumber,
		Timestamp: p.Timestamp,
		SSRC:      p.SSRC,
		Marker:    p.Marker,
		Payload:   primary,
		Redundant: red,
		RawSize:   len(raw),
	}, nil
}
