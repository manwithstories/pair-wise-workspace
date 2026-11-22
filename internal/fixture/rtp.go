package fixture

import (
	"fmt"

	"github.com/pion/rtp"
)

// RTPPacket marshals a single RTP packet header and payload.
func RTPPacket(seq uint16, ts uint32, payloadLen int) []byte {
	p := &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: seq,
			Timestamp: ts, SSRC: 0xDEADBEEF, Marker: payloadLen%2 == 0,
		},
		Payload: make([]byte, payloadLen),
	}
	b, err := p.Marshal()
	if err != nil {
		panic(fmt.Sprintf("fixture rtp marshal: %v", err))
	}
	return b
}
