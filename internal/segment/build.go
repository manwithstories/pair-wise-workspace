package segment

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// Builder assembles synthetic capture and fMP4 payloads for fixtures and
// tests. It is deliberately small and deterministic so generated samples
// hash identically across runs.

// BuildTSRecord builds one RTP record carrying a TS packet that starts a
// PES with the given 33-bit PTS. Video PES payloads carry an IDR NAL when
// sync is true.
func BuildTSRecord(seq uint16, pid uint16, streamID byte, pts int64, sync bool, fill byte) []byte {
	body := []byte{fill}
	if sync {
		body = []byte{0x00, 0x00, 0x00, 0x01, 0x65, fill}
	}
	pes := buildPESAt(streamID, pts, body)
	return buildTSAt(pid, true, pes)
}

func buildPESAt(streamID byte, pts int64, body []byte) []byte {
	enc := EncodePTS(pts)
	out := []byte{0x00, 0x00, 0x01, streamID, 0x00, 0x00, 0x80, 0x80, 5}
	out = append(out, enc[:]...)
	return append(out, body...)
}

func buildTSAt(pid uint16, pusi bool, payload []byte) []byte {
	p := make([]byte, TSPacketLen)
	p[0] = 0x47
	p[1] = byte(pid>>8) & 0x1F
	if pusi {
		p[1] |= 0x40
	}
	p[2] = byte(pid)
	p[3] = 0x10
	copy(p[4:], payload)
	return p
}

// RTPRecord frames a TS packet with a 12-byte RTP header.
func RTPRecord(seq uint16, payload []byte) []byte {
	out := make([]byte, 0, 12+len(payload))
	var h [12]byte
	h[0] = 0x80
	h[1] = 33
	binary.BigEndian.PutUint16(h[2:4], seq)
	out = append(out, h[:]...)
	return append(out, payload...)
}

// RTPParity frames an XOR parity payload with the parity payload type.
func RTPParity(seq, base uint16, parity []byte) []byte {
	out := make([]byte, 0, 14+len(parity))
	var h [12]byte
	h[0] = 0x80
	h[1] = 96
	binary.BigEndian.PutUint16(h[2:4], seq)
	out = append(out, h[:]...)
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], base)
	out = append(out, b[:]...)
	return append(out, parity...)
}

// XOR returns the bytewise XOR of all inputs, truncated to the shortest.
func XOR(parts ...[]byte) []byte {
	if len(parts) == 0 {
		return nil
	}
	out := append([]byte(nil), parts[0]...)
	for _, p := range parts[1:] {
		for i := 0; i < len(out) && i < len(p); i++ {
			out[i] ^= p[i]
		}
	}
	return out
}

// SortSeqs returns sequence numbers in ascending order.
func SortSeqs(m map[uint16][]byte) []uint16 {
	out := make([]uint16, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// MP4Box serializes a box with a 32-bit size.
func MP4Box(typ string, body []byte) []byte {
	out := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(out[:4], uint32(8+len(body)))
	copy(out[4:8], typ)
	copy(out[8:], body)
	return out
}

// BuildMoov builds a minimal moov with one track at the given timescale.
func BuildMoov(trackID uint32, timescale uint32) []byte {
	if timescale == 0 {
		timescale = 90000
	}
	mv := make([]byte, 20)
	binary.BigEndian.PutUint32(mv[12:16], timescale)
	md := make([]byte, 20)
	binary.BigEndian.PutUint32(md[12:16], timescale)
	tk := make([]byte, 16)
	binary.BigEndian.PutUint32(tk[12:16], trackID)
	trak := MP4Box("tkhd", tk)
	trak = append(trak, MP4Box("mdia", MP4Box("mdhd", md))...)
	return MP4Box("moov", append(MP4Box("mvhd", mv), MP4Box("trak", trak)...))
}

// BuildMoof builds a moof with tfhd/tfdt/trun describing count samples of
// fixed duration. When v1 is true tfdt uses the 64-bit version.
func BuildMoof(trackID uint32, base int64, dur uint32, count int, v1 bool) []byte {
	tfhd := make([]byte, 12)
	binary.BigEndian.PutUint32(tfhd[:4], 0x000008) // default sample duration
	binary.BigEndian.PutUint32(tfhd[4:8], trackID)
	binary.BigEndian.PutUint32(tfhd[8:12], dur)

	var tfdt []byte
	if v1 {
		tfdt = make([]byte, 12)
		tfdt[0] = 1
		binary.BigEndian.PutUint64(tfdt[4:12], uint64(base))
	} else {
		tfdt = make([]byte, 8)
		binary.BigEndian.PutUint32(tfdt[4:8], uint32(base))
	}
	trun := make([]byte, 8)
	binary.BigEndian.PutUint32(trun[:4], 0x000001) // data offset present
	binary.BigEndian.PutUint32(trun[4:8], uint32(count))
	trun = append(trun, make([]byte, 4)...)

	body := MP4Box("tfhd", tfhd)
	body = append(body, MP4Box("tfdt", tfdt)...)
	body = append(body, MP4Box("trun", trun)...)
	return MP4Box("moof", MP4Box("traf", body))
}

// SegmentName formats a deterministic segment filename.
func SegmentName(track string, idx int) string {
	return fmt.Sprintf("%s_%05d.m4s", track, idx)
}
