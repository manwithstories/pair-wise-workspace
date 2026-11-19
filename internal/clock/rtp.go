package clock

import (
	"errors"
	"fmt"

	"github.com/pion/rtp"
)

// ArchiveTickExtensionID is the one-byte header extension identifier that
// carries an archive tick in the RTP timestamp extension channel.
const ArchiveTickExtensionID uint8 = 7

// RTPClockRate is the media clock used for the packet's 32-bit timestamp;
// 90 kHz is the standard video clock and divides the 30 kHz grid exactly.
const RTPClockRate uint32 = 90000

var errExtension = errors.New("rtp timestamp extension")

// EncodeRTPPacket builds an RTP packet whose 32-bit timestamp maps exactly
// onto t and whose one-byte header extension additionally carries the full
// 64-bit archive tick, preserving information beyond the RTP cycle.
func EncodeRTPPacket(ssrc uint32, seq uint16, t Tick) (*rtp.Packet, error) {
	ts, err := TickToRTP(t, RTPClockRate)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errExtension, err)
	}
	p := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SSRC:           ssrc,
			SequenceNumber: seq,
			Timestamp:      ts,
		},
		Payload: []byte{0x00},
	}
	if err := p.Header.SetExtension(ArchiveTickExtensionID, tickExtensionBytes(t)); err != nil {
		return nil, fmt.Errorf("%w: %v", errExtension, err)
	}
	return p, nil
}

// DecodeRTPPacket is the inverse: it reads the tick extension when present,
// otherwise it reconstructs ticks from the 32-bit RTP timestamp alone.
func DecodeRTPPacket(raw []byte) (Tick, error) {
	p := &rtp.Packet{}
	if err := p.Unmarshal(raw); err != nil {
		return 0, fmt.Errorf("%w: %v", errExtension, err)
	}
	if p.Extension {
		if data := p.Header.GetExtension(ArchiveTickExtensionID); len(data) == 8 {
			return decodeTickExtensionBytes(data), nil
		}
	}
	return RTPToTick(p.Timestamp, RTPClockRate)
}

func tickExtensionBytes(t Tick) []byte {
	v := uint64(t)
	return []byte{
		byte(v >> 56), byte(v >> 48), byte(v >> 40), byte(v >> 32),
		byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v),
	}
}

func decodeTickExtensionBytes(b []byte) Tick {
	v := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	return Tick(v)
}
