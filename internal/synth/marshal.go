package synth

import (
	"encoding/binary"

	"github.com/pion/rtp"
)

// MarshalRTP encodes one synthetic RTP record as a wire datagram. Video uses
// PT 96 and audio PT 97; both stay below the RTCP 192..223 demux range even
// with the marker bit set.
func MarshalRTP(r Record) ([]byte, error) {
	pt := uint8(AudioPT)
	if r.SSRC == VideoSSRC {
		pt = VideoPT
	}
	p := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         r.Marker,
			PayloadType:    pt,
			SequenceNumber: r.Seq,
			Timestamp:      r.Timestamp,
			SSRC:           r.SSRC,
		},
		Payload: r.Payload,
	}
	return p.Marshal()
}

// MarshalSR encodes one RTCP sender-report record (RFC 3550 §6.4.1), 28
// bytes with no reception report blocks.
func MarshalSR(r Record) []byte {
	b := make([]byte, 28)
	b[0], b[1] = 0x80, SRPT // V=2, RC=0, PT=SR(200)
	b[2], b[3] = 0x00, 0x06 // length 6 -> 28 bytes
	binary.BigEndian.PutUint32(b[4:8], r.SSRC)
	binary.BigEndian.PutUint32(b[8:12], uint32(r.NTPTime>>32))
	binary.BigEndian.PutUint32(b[12:16], uint32(r.NTPTime))
	binary.BigEndian.PutUint32(b[16:20], r.Timestamp)
	return b
}

// MarshalDatagrams converts a full record list into wire datagrams in
// arrival order.
func MarshalDatagrams(recs []Record) ([][]byte, error) {
	out := make([][]byte, 0, len(recs))
	for _, r := range recs {
		if r.Kind == 1 {
			out = append(out, MarshalSR(r))
			continue
		}
		buf, err := MarshalRTP(r)
		if err != nil {
			return nil, err
		}
		out = append(out, buf)
	}
	return out, nil
}
