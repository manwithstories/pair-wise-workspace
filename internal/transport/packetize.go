package transport

import (
	"github.com/pion/rtp"

	"mediad/internal/timeline"
)

// ReceiveDatagram parses one raw RTP datagram and pushes it.
func (r *Receiver) ReceiveDatagram(b []byte, nowNS int64) (duplicate bool, err error) {
	var pkt rtp.Packet
	if err := pkt.Unmarshal(b); err != nil {
		return false, timeline.Wrap(timeline.KindArgument, "transport.rtp.unmarshal", -1, -1, err)
	}
	return r.Push(&pkt, nowNS)
}

// PacketizeAU splits one access unit into RTP payload chunks. Sequence
// numbers are allocated continuously from nextSeq and the final packet has
// the marker bit set.
func PacketizeAU(timestamp uint32, au []byte, mtuPayload int, nextSeq uint16, ssrc uint32) []rtp.Packet {
	if mtuPayload <= 0 {
		mtuPayload = 1200
	}
	if len(au) == 0 {
		return []rtp.Packet{{Header: rtp.Header{
			Version: 2, Marker: true, SequenceNumber: nextSeq, Timestamp: timestamp, SSRC: ssrc,
		}}}
	}
	var out []rtp.Packet
	for off := 0; off < len(au); off += mtuPayload {
		end := off + mtuPayload
		if end > len(au) {
			end = len(au)
		}
		last := end == len(au)
		out = append(out, rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Marker:         last,
				SequenceNumber: nextSeq + uint16(len(out)),
				Timestamp:      timestamp,
				SSRC:           ssrc,
			},
			Payload: append([]byte(nil), au[off:end]...),
		})
	}
	return out
}
