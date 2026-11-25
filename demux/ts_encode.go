package demux

import (
	"bytes"

	"fieldpost/internal/core"
)

// MuxTS packetizes synthetic AVC/AAC access units into 188-byte TS packets
// with PTS/DTS. It exists to generate self-contained fixtures and round-trip
// tests; it does not transcode essence.
func MuxTS(seg *core.Segment) ([]byte, error) {
	var out bytes.Buffer
	cc := map[uint16]int{}
	for _, sm := range seg.Samples {
		pid, streamID := tsStream(sm.TrackID, seg)
		pes := encodePES(streamID, sm.PTS, sm.DTS, sm.Data)
		chunks := splitPayload(pes, 184)
		for ci, chunk := range chunks {
			pusi := ci == 0
			pkt := make([]byte, TSPacketSize)
			pkt[0] = 0x47
			pkt[1] = byte(pid >> 8 & 0x1f)
			pkt[2] = byte(pid & 0xff)
			if pusi {
				pkt[1] |= 0x40
			}
			control := byte(0x01) // payload only
			payload := chunk
			headerLen := 4
			if len(chunk) < 184 {
				pad := 184 - len(chunk)
				control = 0x03 // adaptation + payload
				pkt[4] = byte(pad - 1)
				for i := 5; i < 5+int(pkt[4]); i++ {
					pkt[i] = 0xff
				}
				headerLen = 5 + int(pkt[4])
				// If adaptation length is zero, headerLen formula gives 5 but
				// the single length byte is already the complete field.
				if pad == 1 {
					headerLen = 5
				}
			}
			pkt[3] = (control << 4) | byte(cc[pid]&0x0f)
			cc[pid] = (cc[pid] + 1) & 0x0f
			copy(pkt[headerLen:], payload)
			out.Write(pkt)
		}
	}
	return out.Bytes(), nil
}

func tsStream(trackID uint32, seg *core.Segment) (uint16, byte) {
	kind := core.TrackVideo
	if seg != nil {
		if tr, ok := seg.TrackByID(trackID); ok {
			kind = tr.Kind
		}
	}
	if kind == core.TrackAudio {
		return tsPIDAudio, 0xc0
	}
	return tsPIDVideo, 0xe0
}

func splitPayload(b []byte, max int) [][]byte {
	if len(b) == 0 {
		return [][]byte{{}}
	}
	var out [][]byte
	for len(b) > 0 {
		n := minInt(len(b), max)
		out = append(out, b[:n])
		b = b[n:]
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func encodePES(streamID byte, pts, dts core.Ticks, payload []byte) []byte {
	both := pts != dts
	headerDataLen := byte(5)
	flags2 := byte(0x80) // PTS only
	if both {
		headerDataLen = 10
		flags2 = 0xc0
	}
	pesPayloadLen := 3 + int(headerDataLen) + len(payload)
	var b bytes.Buffer
	b.Write([]byte{0x00, 0x00, 0x01, streamID})
	b.WriteByte(byte(pesPayloadLen >> 8))
	b.WriteByte(byte(pesPayloadLen))
	b.WriteByte(0x80)
	b.WriteByte(flags2)
	b.WriteByte(headerDataLen)
	if both {
		b.Write(encodePTSField(0x03, pts))
		b.Write(encodePTSField(0x01, dts))
	} else {
		b.Write(encodePTSField(0x02, pts))
	}
	b.Write(payload)
	return b.Bytes()
}

func encodePTSField(marker byte, t core.Ticks) []byte {
	v := uint64(t)
	return []byte{
		marker<<4 | byte((v>>30)&0x07)<<1 | 0x01,
		byte((v >> 22) & 0xff),
		byte((v>>14)&0xfe) | 0x01,
		byte((v >> 7) & 0xff),
		byte((v<<1)&0xfe) | 0x01,
	}
}
