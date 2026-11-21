package rtp

import (
	"testing"

	pionrtp "github.com/pion/rtp"
)

// stream builds n packets with the given sequence and timestamp policy.
func packet(seq uint16, ts uint32) *pionrtp.Packet {
	return &pionrtp.Packet{
		Header:  pionrtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, Marker: true},
		Payload: []byte{1, 2, 3, 4},
	}
}

func feed(t *testing.T, r *Receiver, pkts []*pionrtp.Packet) []Packet {
	t.Helper()
	var out []Packet
	for _, p := range pkts {
		got, err := r.Push(p)
		if err != nil {
			t.Fatalf("push seq %d: %v", p.SequenceNumber, err)
		}
		out = append(out, got...)
	}
	return out
}
