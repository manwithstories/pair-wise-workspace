package rtp

import "time"

func pkt(seq uint16, payload string, at time.Duration) Packet {
	return Packet{Seq: seq, Payload: []byte(payload), Arrival: at}
}
