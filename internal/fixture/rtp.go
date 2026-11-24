package fixture

import (
	"encoding/binary"
	"math/rand"

	"github.com/pion/rtp"
)

// RTPPacketSpec controls one generated RTP packet.
type RTPPacketSpec struct {
	// Timestamp is the raw RTP timestamp; a non-zero first value models a
	// non-zero track origin that the demuxer must preserve.
	Timestamp uint32
	Seq       uint16
	SSRC      uint32
	Marker    bool
	PayloadN  int
}

// BuildRTPCapture writes the length-prefixed RTP frame format consumed by
// demux.RTPReader. Payloads are deterministic from the packet index.
func BuildRTPCapture(specs []RTPPacketSpec) []byte {
	var out []byte
	for i, sp := range specs {
		payload := deterministicPayload(nil, 9, i, sp.PayloadN)
		pkt := rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: sp.Seq,
				Timestamp:      sp.Timestamp,
				SSRC:           sp.SSRC,
				Marker:         sp.Marker,
			},
			Payload: payload,
		}
		raw, err := pkt.Marshal()
		if err != nil {
			panic(err)
		}
		hdr := make([]byte, 4)
		binary.BigEndian.PutUint32(hdr, uint32(len(raw)))
		out = append(out, hdr...)
		out = append(out, raw...)
	}
	return out
}

// RTPSequence builds a monotonic packet spec list starting at startTS with a
// fixed tick step. Every markerEvery-th packet sets the marker bit.
func RTPSequence(n int, startTS, step uint32, ssrc uint32, markerEvery int) []RTPPacketSpec {
	out := make([]RTPPacketSpec, n)
	for i := 0; i < n; i++ {
		marker := markerEvery > 0 && (i%markerEvery == 0)
		out[i] = RTPPacketSpec{
			Timestamp: startTS + uint32(i)*step,
			Seq:       uint16(i),
			SSRC:      ssrc,
			Marker:    marker,
			PayloadN:  24,
		}
	}
	return out
}

// SeededRand returns the deterministic random source shared by a run.
func SeededRand(seed int64) *rand.Rand {
	return rand.New(rand.NewSource(seed))
}
