// Package rtpadapter maps timed media blocks to Pion RTP packets.
package rtpadapter

import (
	"github.com/pion/rtp"

	"mediaproc/internal/demux"
	"mediaproc/internal/model"
)

const defaultMTU = 1200

// Packetizer creates deterministic RTP headers from the shared nanosecond clock.
type Packetizer struct {
	PayloadType uint8
	SSRC        uint32
	ClockRate   uint32
	MTU         int
	sequence    uint16
}

// NewOpusPacketizer returns a 48 kHz Opus RTP packetizer.
func NewOpusPacketizer(ssrc uint32) *Packetizer {
	return &Packetizer{PayloadType: 111, SSRC: ssrc, ClockRate: 48000, MTU: defaultMTU}
}

// Sequence returns the next sequence number that will be used.
func (p *Packetizer) Sequence() uint16 { return p.sequence }

// Packetize splits one block frame into MTU-sized RTP packets.
func (p *Packetizer) Packetize(b *demux.Block) ([]*rtp.Packet, error) {
	if b == nil {
		return nil, model.NewError(model.PhaseRTP, -1, false, "nil block", nil)
	}
	if p.MTU <= 0 {
		p.MTU = defaultMTU
	}
	frame := b.Data
	if len(b.Frames) > 0 {
		frame = b.Frames[0]
	}
	if len(frame) == 0 {
		return nil, model.NewError(model.PhaseRTP, b.Offset, true, "empty RTP frame", nil)
	}

	ts := b.TimeNS.RTPTimestamp(p.ClockRate)
	var out []*rtp.Packet
	for start := 0; start < len(frame); start += p.MTU {
		end := min(start+p.MTU, len(frame))
		payload := frame[start:end]
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Padding:        false,
				Extension:      false,
				Marker:         end == len(frame),
				PayloadType:    p.PayloadType,
				SequenceNumber: p.sequence,
				Timestamp:      ts,
				SSRC:           p.SSRC,
			},
			Payload: append([]byte(nil), payload...),
		}
		raw, err := pkt.Marshal()
		if err != nil {
			return nil, model.NewError(model.PhaseRTP, b.Offset, false, "marshal RTP", err)
		}
		var decoded rtp.Packet
		if err := decoded.Unmarshal(raw); err != nil {
			return nil, model.NewError(model.PhaseRTP, b.Offset, false, "roundtrip RTP", err)
		}
		out = append(out, pkt)
		p.sequence++
	}
	return out, nil
}
