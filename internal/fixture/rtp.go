package fixture

import (
	"encoding/binary"

	"github.com/pion/rtp"

	"mediascan/internal/repair"
)

// RTPOptions configures the generated RTP stream.
type RTPOptions struct {
	Packets   int
	DropEvery int
	Seed      int64
}

// GenerateRTP builds a packet stream in which every packet carries a
// redundant copy of its predecessor. When DropEvery is positive every Nth
// packet is omitted, so isolated losses are recoverable while runs of losses
// remain visible as gaps.
func GenerateRTP(opt RTPOptions) [][]byte {
	if opt.Packets <= 0 {
		return nil
	}
	var out [][]byte
	var prev []byte
	for i := 0; i < opt.Packets; i++ {
		payload := rtpBody(uint16(i), opt.Seed)
		var red *repair.Redundant
		if prev != nil {
			red = &repair.Redundant{Seq: uint16(i - 1), Bytes: prev}
		}
		drop := opt.DropEvery > 0 && (i+1)%opt.DropEvery == 0
		if !drop {
			p := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					SequenceNumber: uint16(i),
					Timestamp:      uint32(i * 3000),
					SSRC:           0x5151,
					Marker:         true,
				},
				Payload: repair.EncodePayload(payload, red),
			}
			raw, err := p.Marshal()
			if err == nil {
				out = append(out, raw)
			}
		}
		prev = payload
	}
	return out
}

// rtpBody is the opaque primary payload of packet i.
func rtpBody(i uint16, seed int64) []byte {
	n := 32 + int((uint32(i)*7+uint32(seed))%48)
	b := make([]byte, n)
	binary.BigEndian.PutUint16(b, i)
	for j := 2; j < n; j++ {
		b[j] = byte((int(i)*13 + j*7 + int(seed)) & 0xff)
	}
	return b
}
