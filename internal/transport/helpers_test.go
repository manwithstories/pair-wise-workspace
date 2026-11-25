package transport

import "github.com/pion/rtp"

// rtpHeader builds a minimal packet carrying only the sequence number.
func rtpHeader(seq uint16) rtp.Packet {
	return rtp.Packet{Header: rtp.Header{
		Version: 2, SequenceNumber: seq, SSRC: 1,
	}}
}
