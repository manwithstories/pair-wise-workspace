package pipeline

import (
	"github.com/pion/rtp"

	"github.com/archive/rtprestore/internal/synth"
)

// toPacket converts a synthetic RTP record into the pion packet type the
// reorderer consumes, assigning the capture's fixed payload types.
func toPacket(r synth.Record) *rtp.Packet {
	pt := uint8(synth.AudioPT)
	if r.SSRC == synth.VideoSSRC {
		pt = synth.VideoPT
	}
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         r.Marker,
			PayloadType:    pt,
			SequenceNumber: r.Seq,
			Timestamp:      r.Timestamp,
			SSRC:           r.SSRC,
		},
		Payload: append([]byte(nil), r.Payload...),
	}
}
