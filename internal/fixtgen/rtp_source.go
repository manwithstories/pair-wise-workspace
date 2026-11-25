package fixtgen

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"

	"github.com/pion/rtp"
)

// WriteRTPSource renders an H.264-over-RTP capture stream: 2-byte length
// prefix per datagram, IDR NAL units split across FU-A packets. For the
// rtp_truncated segment the final FU-A is deliberately cut.
func WriteRTPSource(dir string, seg Segment) (string, error) {
	var stream bytes.Buffer
	ssrc := uint32(0xA11CE000 + seg.Index)
	ts := uint32(0)
	seq := uint16(0)

	for s := 0; s < SamplesPerTrack; s++ {
		isKey := s%KeyEvery == 0
		nal := makeSample(seg.Index, 0, s, "avc1")
		// Strip the AVCC length prefix to get the bare NAL for RTP.
		nal = nal[4:]
		packets := fuPackets(nal, isKey)
		for i, payload := range packets {
			// Drop the final FU-A packet of the final sample for the truncation fault.
			if seg.Fault == FaultRTPTruncated && s == SamplesPerTrack-1 && i == len(packets)-1 {
				continue
			}
			pkt := &rtp.Packet{
				Payload: payload,
			}
			pkt.Version = 2
			pkt.PayloadType = 96
			pkt.SSRC = ssrc
			pkt.Timestamp = ts
			pkt.SequenceNumber = seq
			pkt.Marker = i == len(packets)-1
			seq++
			raw, err := pkt.Marshal()
			if err != nil {
				return "", err
			}
			lp := make([]byte, 2)
			binary.BigEndian.PutUint16(lp, uint16(len(raw)))
			stream.Write(lp)
			stream.Write(raw)
		}
		ts += 3000 // 90 kHz clock, ~33.3 ms per frame
	}

	path := filepath.Join(dir, seg.ID+".rtp")
	return path, os.WriteFile(path, stream.Bytes(), 0o644)
}

// fuPackets splits one NAL into STAP-A-free FU-A packets. Small NALs are sent
// as a single NAL unit packet; larger ones are fragmented with a 12-byte cap.
func fuPackets(nal []byte, isKey bool) [][]byte {
	if len(nal) <= 14 {
		return [][]byte{append([]byte(nil), nal...)}
	}
	indicator := (nal[0] & 0xE0) | nalTypeFUA
	origHeader := nal[0]
	nalType := origHeader & 0x1F
	const chunk = 12
	var out [][]byte
	body := nal[1:]
	for off := 0; off < len(body); off += chunk {
		end := off + chunk
		if end > len(body) {
			end = len(body)
		}
		fuHeader := nalType
		if off == 0 {
			fuHeader |= 0x80 // start bit
		}
		if end == len(body) {
			fuHeader |= 0x40 // end bit
		}
		pkt := []byte{indicator, fuHeader}
		pkt = append(pkt, body[off:end]...)
		out = append(out, pkt)
	}
	_ = isKey
	return out
}

const nalTypeFUA = 28
