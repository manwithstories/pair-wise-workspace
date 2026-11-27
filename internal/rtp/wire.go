package rtp

import (
	"time"

	pionrtp "github.com/pion/rtp"

	"tsqc/internal/clock"
)

// PayloadType is the dynamic payload type used for the multiplexed MPEG-TS.
const PayloadType uint8 = 33

// SSRC identifies the single synthetic stream produced by the fixtures.
const SSRC uint32 = 0x54535143

// DecodeWire unmarshals a Pion RTP packet into the internal representation.
// The arrival timestamp is supplied by the caller because RTP itself has no
// wall-clock field.
func DecodeWire(raw []byte, arrival time.Duration) (Packet, error) {
	var p pionrtp.Packet
	if err := p.Unmarshal(raw); err != nil {
		return Packet{}, clock.RecoverableError(clock.CodeSequence, "rtp.DecodeWire", "malformed RTP: %v", err)
	}
	return Packet{
		Seq:         p.SequenceNumber,
		PayloadType: p.PayloadType,
		Marker:      p.Marker,
		Timestamp:   p.Timestamp,
		SSRC:        p.SSRC,
		Payload:     append([]byte(nil), p.Payload...),
		Arrival:     arrival,
	}, nil
}

// EncodeWire marshals an internal packet with Pion so fixtures and any
// on-the-wire producer share one serialization.
func EncodeWire(p Packet) ([]byte, error) {
	pt := p.PayloadType
	if pt == 0 {
		pt = PayloadType
	}
	ssrc := p.SSRC
	if ssrc == 0 {
		ssrc = SSRC
	}
	out, err := (&pionrtp.Packet{
		Header: pionrtp.Header{
			Version:        2,
			PayloadType:    pt,
			SequenceNumber: p.Seq,
			Timestamp:      p.Timestamp,
			SSRC:           ssrc,
			Marker:         p.Marker,
		},
		Payload: p.Payload,
	}).Marshal()
	if err != nil {
		return nil, clock.RecoverableError(clock.CodeSequence, "rtp.EncodeWire", "marshal: %v", err)
	}
	return out, nil
}

// PushWire decodes one wire packet and feeds the receiver.
func (r *Receiver) PushWire(raw []byte, arrival time.Duration) ([]Packet, []GapRecord, error) {
	p, err := DecodeWire(raw, arrival)
	if err != nil {
		return nil, nil, err
	}
	out, recs := r.Push(p)
	return out, recs, nil
}
