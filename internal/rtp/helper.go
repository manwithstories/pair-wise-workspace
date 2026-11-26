package rtp

import "github.com/pion/rtp"

func rtpPacketWithTimestamp(seq uint16, timestamp uint32, payload []byte) rtp.Packet {
	return rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      timestamp,
		},
		Payload: payload,
	}
}
