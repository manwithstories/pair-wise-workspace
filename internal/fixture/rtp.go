package fixture

import (
	"github.com/pion/rtp"
)

// RTPSpec describes a synthetic RTP packet stream.
type RTPSpec struct {
	SSRC       uint32
	PayloadTyp uint8
	StartSeq   uint16
	BaseTS     uint32
	TSInc      uint32
	Count      int
	MTU        int
}

// BuildRTP generates a deterministic RTP packet stream. Each packet carries
// a small deterministic payload; no real media encoding is involved.
func BuildRTP(spec RTPSpec) ([][]byte, error) {
	out := make([][]byte, 0, spec.Count)
	for i := 0; i < spec.Count; i++ {
		payload := payload(int64(spec.SSRC), i, i%7 == 0, 4)
		if spec.MTU > 0 && len(payload) > spec.MTU-12 {
			payload = payload[:spec.MTU-12]
		}
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    spec.PayloadTyp,
				SequenceNumber: spec.StartSeq + uint16(i),
				Timestamp:      spec.BaseTS + uint32(i)*spec.TSInc,
				SSRC:           spec.SSRC,
				Marker:         i%7 == 6,
			},
			Payload: payload,
		}
		raw, err := pkt.Marshal()
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}
