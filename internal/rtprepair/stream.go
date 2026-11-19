package rtprepair

import (
	"fmt"

	"github.com/pion/rtp"
)

// Emissions counts packets the repairer eventually released.
type Emissions struct {
	Packets []*Packet
}

// Simulate feeds datagrams through a Repairer and returns the emitted slice.
// A nil packet from Parse is skipped, so malformed datagrams degrade rather
// than abort the stream.
func Simulate(raws [][]byte, opts Options, file string) ([]*Packet, *Repairer) {
	r := New(opts, file)
	var out []*Packet
	for i, raw := range raws {
		p, err := r.Parse(raw, int64(i), int64(i))
		if err != nil {
			continue
		}
		out = append(out, r.Push(p)...)
	}
	out = append(out, r.Flush()...)
	return out, r
}

// Encode builds a wire-format RTP datagram for test fixtures.
func Encode(seq uint16, ts uint32, ssrc uint32, payload []byte) []byte {
	pkt := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
		},
		Payload: payload,
	}
	buf := make([]byte, pkt.MarshalSize())
	n, err := pkt.MarshalTo(buf)
	if err != nil {
		panic(fmt.Sprintf("rtprepair: encode fixture packet: %v", err))
	}
	return buf[:n]
}
