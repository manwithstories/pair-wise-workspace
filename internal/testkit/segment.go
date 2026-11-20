// Package testkit deterministically generates normal and damaged media
// fixtures — HLS playlists, fMP4 segments, RTP streams — so the pipeline
// can be exercised without any external assets.
package testkit

import "encoding/binary"

// Box builds a single 8-byte-header MP4 box.
func Box(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

func fullBox(typ string, version byte, payload []byte) []byte {
	body := make([]byte, 4+len(payload))
	body[0] = version
	copy(body[4:], payload)
	return Box(typ, body)
}

// MediaSegment builds a minimal fMP4 media segment: styp (msix brand when
// the segment starts on a keyframe), moof(mfhd+traf(tfdt)), mdat.
func MediaSegment(seq uint32, decodeTime uint64, keyframe bool, payload []byte) []byte {
	// styp
	stypPayload := make([]byte, 0, 16)
	stypPayload = append(stypPayload, "msdh"...)
	stypPayload = append(stypPayload, 0, 0, 0, 0)
	stypPayload = append(stypPayload, "msdh"...)
	if keyframe {
		stypPayload = append(stypPayload, "msix"...)
	}
	styp := Box("styp", stypPayload)
	// mfhd
	mfhdBody := make([]byte, 4)
	binary.BigEndian.PutUint32(mfhdBody, seq)
	mfhd := fullBox("mfhd", 0, mfhdBody)
	// tfdt, version 1: 64-bit baseMediaDecodeTime
	tfdtBody := make([]byte, 8)
	binary.BigEndian.PutUint64(tfdtBody, decodeTime)
	tfdt := fullBox("tfdt", 1, tfdtBody)
	traf := Box("traf", tfdt)
	moof := Box("moof", append(mfhd, traf...))
	mdat := Box("mdat", payload)
	out := make([]byte, 0, len(styp)+len(moof)+len(mdat))
	out = append(out, styp...)
	out = append(out, moof...)
	out = append(out, mdat...)
	return out
}

// InitSegment builds a minimal init segment: ftyp + empty moov.
func InitSegment() []byte {
	ftyp := Box("ftyp", []byte("isom\x00\x00\x00\x00isom"))
	return append(ftyp, Box("moov", nil)...)
}

// CorruptLength inflates the size field of the first box whose type
// matches, making it overrun the buffer (recoverable structural damage).
func CorruptLength(data []byte, typ string, extra uint32) []byte {
	out := append([]byte(nil), data...)
	for off := 0; off+8 <= len(out); {
		size := binary.BigEndian.Uint32(out[off : off+4])
		if string(out[off+4:off+8]) == typ {
			binary.BigEndian.PutUint32(out[off:off+4], size+extra)
			return out
		}
		if size < 8 || off+int(size) > len(out) {
			return out
		}
		off += int(size)
	}
	return out
}

// BadLength rewrites the first box header to a size below its header,
// producing an unrecoverable structural error.
func BadLength(data []byte) []byte {
	out := append([]byte(nil), data...)
	if len(out) >= 8 {
		binary.BigEndian.PutUint32(out[0:4], 4)
	}
	return out
}
