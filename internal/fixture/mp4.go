package fixture

import (
	"encoding/binary"
)

// Minimal big-endian box builders mirroring the subset the cmaf parser
// accepts. They live in the fixture package so test data never imports
// production construction helpers.

func box(typ string, payload []byte) []byte {
	size := uint32(8 + len(payload))
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out, size)
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

func fullBox(typ string, version byte, flags uint32, body []byte) []byte {
	hdr := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append(hdr, body...))
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

const (
	trunDataOffset     = 0x000001
	trunSampleDuration = 0x000100
	trunSampleSize     = 0x000200
)

// BuildFragment constructs one moof/mdat fragment pair for a single track,
// with VFR sample durations and explicit payload sizes.
func BuildFragment(seq uint32, track TrackSpec) []byte {
	mfhd := fullBox("mfhd", 0, 0, u32(seq))
	tfhd := fullBox("tfhd", 0, 0, u32(track.ID))
	tfdt := fullBox("tfdt", 1, 0, u64(track.BaseTfdt))

	flags := trunDataOffset | trunSampleDuration | trunSampleSize
	var body []byte
	body = append(body, u32(uint32(len(track.Samples)))...)
	body = append(body, u32(0)...) // data offset placeholder
	var payloadLen uint32
	for _, s := range track.Samples {
		body = append(body, u32(s.Duration)...)
		body = append(body, u32(s.Size)...)
		payloadLen += s.Size
	}
	trun := fullBox("trun", 1, uint32(flags), body)
	traf := box("traf", append(append(append([]byte{}, tfhd...), tfdt...), trun...))
	moof := box("moof", append(append([]byte{}, mfhd...), traf...))

	payload := make([]byte, payloadLen)
	for i := range payload {
		payload[i] = byte((i*31 + int(track.ID)*7) & 0xff)
	}
	mdat := box("mdat", payload)
	stream := append(moof, mdat...)

	// Patch data offset: from moof start to the first mdat payload byte.
	dataOff := uint32(len(moof) + 8)
	trunStart := indexTag(stream, "trun") - 4
	// payload layout: fullbox(4) + count(4) + dataOffset(4).
	binary.BigEndian.PutUint32(stream[trunStart+8+8:], dataOff)
	return stream
}

// indexTag returns the position of a 4-char type tag, or -1.
func indexTag(haystack []byte, typ string) int {
	for i := 0; i+4 <= len(haystack); i++ {
		if string(haystack[i:i+4]) == typ {
			return i
		}
	}
	return -1
}

// TruncateMdat cuts the stream inside the mdat payload so the declared box
// length can no longer be satisfied. cutAt is an absolute byte offset.
func TruncateMdat(stream []byte, cutAt int) []byte {
	if cutAt < 0 || cutAt > len(stream) {
		cutAt = len(stream)
	}
	out := make([]byte, cutAt)
	copy(out, stream[:cutAt])
	return out
}
