package sample

import (
	"github.com/pion/rtp"

	mrtp "medialab/internal/rtp"
)

// RTPStartSequence is chosen near 65535 so the 12-packet main stream wraps.
const RTPStartSequence uint16 = 65530

// RTPTimestampStep is approximately one 30 fps frame at a 90 kHz clock.
const RTPTimestampStep uint32 = 3000

// BuildRTPStream packetizes payloads on a wrapping sequence/timestamp basis and
// assigns real cumulative offsets in the concatenated raw RTP byte stream.
func BuildRTPStream(payloads [][]byte) ([]*mrtp.InPacket, [][]byte, error) {
	packets := make([]*mrtp.InPacket, 0, len(payloads))
	wire := make([][]byte, 0, len(payloads))
	offset := int64(0)
	tsBase := ^uint32(0) - 2*RTPTimestampStep
	for i, payload := range payloads {
		p := rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: RTPStartSequence + uint16(i),
				Timestamp:      tsBase + uint32(i)*RTPTimestampStep,
				SSRC:           0x00017385,
			},
			Payload: append([]byte(nil), payload...),
		}
		raw, err := p.Marshal()
		if err != nil {
			return nil, nil, err
		}
		packets = append(packets, &mrtp.InPacket{P: p, TrackID: 1, RawOffset: offset})
		wire = append(wire, raw)
		offset += int64(len(raw))
	}
	return packets, wire, nil
}

// ClonePacket returns a deep-ish copy suitable for duplicate/conflict fixtures.
func ClonePacket(p *mrtp.InPacket, payload []byte) *mrtp.InPacket {
	cp := *p
	cp.P = *p.P.Clone()
	if payload != nil {
		cp.P.Payload = append([]byte(nil), payload...)
	} else {
		cp.P.Payload = append([]byte(nil), p.P.Payload...)
	}
	return &cp
}
