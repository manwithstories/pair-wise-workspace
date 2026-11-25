package fixture

import (
	"encoding/binary"

	"github.com/pion/rtp"

	"mediqc/internal/media"
	"mediqc/internal/remux"
)

const pcrMod = uint64(1) << 33

func withCCJump(src []byte, pid uint16) []byte {
	out := append([]byte(nil), src...)
	var matches []int
	for off := 0; off+188 <= len(out); off += 188 {
		p := uint16(out[off+1]&0x1f)<<8 | uint16(out[off+2])
		if p == pid {
			matches = append(matches, off)
		}
	}
	if len(matches) < 8 {
		return out
	}
	start := matches[len(matches)/3]
	for off := start; off+188 <= len(out); off += 188 {
		p := uint16(out[off+1]&0x1f)<<8 | uint16(out[off+2])
		if p != pid {
			continue
		}
		cc := out[off+3] & 0x0f
		out[off+3] = (out[off+3] & 0xf0) | ((cc + 2) & 0x0f)
	}
	return out
}

func injectPCRWrap(src []byte) []byte {
	out := append([]byte(nil), src...)
	var candidates []int
	for off := 0; off+188 <= len(out); off += 188 {
		afc := (out[off+3] >> 4) & 0x03
		if afc != 2 && afc != 3 {
			continue
		}
		alen := int(out[off+4])
		if alen >= 7 && off+5+alen <= len(out) && out[off+5] == 0x00 {
			candidates = append(candidates, off)
		}
	}
	if len(candidates) < 3 {
		return out
	}
	picks := []int{candidates[0], candidates[len(candidates)/2], candidates[len(candidates)-1]}
	values := []uint64{pcrMod - 6000, pcrMod - 3000, 3000}
	for i, off := range picks {
		out[off+5] = 0x10 // PCR flag
		putPCR(out[off+6:off+12], values[i])
	}
	return out
}

func putPCR(b []byte, base uint64) {
	b[0] = byte(base >> 25)
	b[1] = byte(base >> 17)
	b[2] = byte(base >> 9)
	b[3] = byte(base >> 1)
	b[4] = byte((base&1)<<7) | 0x7e
	b[5] = 0x00
}

func withPSIUpdate(src []byte, tracks []media.Track, samples []media.Sample) []byte {
	update, merr := remux.BuildTS(tracks, samples, remux.TSOptions{PMTVersion: 1})
	if merr != nil || len(update) < 2*188 {
		return src
	}
	corrupt := append([]byte(nil), update[188:2*188]...)
	valid := append([]byte(nil), update[188:2*188]...)

	// Section starts immediately after the PSI pointer byte at packet offset 4.
	sectionLen := int(binary.BigEndian.Uint16(corrupt[6:8])&0x0fff) + 3
	crcLast := 5 + sectionLen - 1
	if crcLast < len(corrupt) {
		corrupt[crcLast] ^= 0xff
	}
	corrupt[3] = (corrupt[3] & 0xf0) | 0x01
	valid[3] = (valid[3] & 0xf0) | 0x02
	return append(append(append(src, corrupt...), valid...))
}

func packetizeRTPTS(ts []byte, seed int64) ([][]byte, *media.Error) {
	const payloadUnits = 7
	chunk := payloadUnits * 188
	seq := uint16(seed & 0xffff)
	ssrc := uint32((seed * 1_103_515_245) & 0xffffffff)
	var out [][]byte
	for off := 0; off < len(ts); off += chunk {
		end := off + chunk
		if end > len(ts) {
			end = len(ts)
		}
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    33, // MP2T
				SequenceNumber: seq,
				Timestamp:      uint32(seed) + uint32(off/188)*900,
				SSRC:           ssrc,
				Marker:         end == len(ts),
			},
			Payload: ts[off:end],
		}
		b, err := pkt.Marshal()
		if err != nil {
			return nil, &media.Error{Code: media.CodeInternal, Component: "fixture", Message: err.Error()}
		}
		out = append(out, b)
		seq++
	}
	return out, nil
}
