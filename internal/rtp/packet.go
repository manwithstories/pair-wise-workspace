// Package rtp simulates RTP delivery faults (loss, reordering, duplication,
// sequence wraparound and jitter) with bounded buffers and a recoverable,
// deterministic receiver.
package rtp

import (
	"fmt"

	pionrtp "github.com/pion/rtp"

	"mediasim/internal/clock"
)

// Packet is one simulated media packet. Wire carries the real pion header so
// the simulation exercises the same parsing a receiver would.
type Packet struct {
	Wire        []byte
	Seq         uint16
	TS          uint32
	Frame       int
	Keyframe    bool
	LossArrival int64 // nanoseconds after capture; jitter perturbs this
}

// Encode serialises the synthetic payload into a pion RTP packet.
func (p *Packet) Encode(ssrc uint32) error {
	pk := pionrtp.Packet{
		Header: pionrtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: p.Seq,
			Timestamp: p.TS, SSRC: ssrc, Marker: p.Keyframe,
		},
		Payload: []byte{
			'R', 'S', 'M', 'P',
			byte(p.Frame >> 24), byte(p.Frame >> 16), byte(p.Frame >> 8), byte(p.Frame),
			byte(p.TS >> 24), byte(p.TS >> 16), byte(p.TS >> 8), byte(p.TS),
		},
	}
	if p.Keyframe {
		pk.Payload = append(pk.Payload, 1)
	} else {
		pk.Payload = append(pk.Payload, 0)
	}
	buf, err := pk.Marshal()
	if err != nil {
		return err
	}
	p.Wire = buf
	return nil
}

// Decode parses a wire packet back into the synthetic view.
func Decode(wire []byte) (Packet, error) {
	var pk pionrtp.Packet
	if err := pk.Unmarshal(wire); err != nil {
		return Packet{}, clock.NewError(clock.CatTransport, clock.Span{}, clock.RecoveredSkipped, "rtp unmarshal", err)
	}
	if len(pk.Payload) < 13 || string(pk.Payload[:4]) != "RSMP" {
		return Packet{}, clock.Errorf(clock.CatTransport, clock.Span{}, clock.RecoveredSkipped, "short or foreign payload")
	}
	frame := int(pk.Payload[4])<<24 | int(pk.Payload[5])<<16 | int(pk.Payload[6])<<8 | int(pk.Payload[7])
	return Packet{
		Wire: wire, Seq: pk.SequenceNumber, TS: pk.Timestamp,
		Frame: frame, Keyframe: pk.Payload[12] == 1,
	}, nil
}

// seqLT compares two sequence numbers on a wrapping circle.
func seqLT(a, b uint16) bool { return int16(a-b) < 0 }

// seqDiff is b-a modulo 2^16.
func seqDiff(a, b uint16) int { return int(int16(b - a)) }

func (p Packet) String() string {
	return fmt.Sprintf("seq=%d frame=%d key=%v", p.Seq, p.Frame, p.Keyframe)
}
