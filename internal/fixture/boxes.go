package fixture

import (
	"encoding/binary"
)

// concat flattens byte slices.
func concat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// flatten joins a slice of byte slices.
func flatten(chunks [][]byte) []byte { return concat(chunks...) }

// totalLen sums the lengths of payloads.
func totalLen(chunks [][]byte) int {
	n := 0
	for _, c := range chunks {
		n += len(c)
	}
	return n
}

func u16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

// mp4Box wraps payload in a box with a 32-bit size.
func mp4Box(typ string, payload []byte) []byte {
	size := uint32(8 + len(payload))
	return concat(u32(size), []byte(typ), payload)
}

// fullBox wraps payload in a FullBox with version and flags.
func fullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	vf := make([]byte, 4)
	vf[0] = version
	vf[1] = byte(flags >> 16)
	vf[2] = byte(flags >> 8)
	vf[3] = byte(flags)
	return mp4Box(typ, concat(vf, payload))
}

// mvhd builds a version-0 movie header.
func (g *mp4Gen) mvhd(timescale uint32, duration uint32) []byte {
	body := concat(
		u32(0), u32(0), // creation, modification
		u32(timescale), u32(duration),
		u32(0x00010000), // rate 1.0
		u16(0x0100),     // volume 1.0
		u16(0),          // reserved
		u32(0), u32(0),  // reserved
		matrix(),                                       // 9×4 matrix
		u32(0), u32(0), u32(0), u32(0), u32(0), u32(0), // pre_defined [6]int32
		u32(2), // next_track_ID
	)
	return fullBox("mvhd", 0, 0, body)
}

func matrix() []byte {
	// Identity matrix, fixed point 16.16 / 2.30.
	m := []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	out := make([]byte, 0, 36)
	for _, v := range m {
		out = append(out, u32(v)...)
	}
	return out
}

// tkhd builds a version-0 track header.
func tkhd(trackID uint32, duration uint32, width, height uint16) []byte {
	body := concat(
		u32(0), u32(0),
		u32(trackID), u32(0),
		u32(duration),
		u32(0), u32(0), // reserved
		u16(0), u16(0), // layer, alternate_group
		u16(0), u16(0), // volume, reserved
		matrix(),
		u32(uint32(width)<<16), u32(uint32(height)<<16),
	)
	return fullBox("tkhd", 0, 0x000007, body)
}

// mdhd builds a version-0 media header.
func (g *mp4Gen) mdhd(timescale, duration uint32) []byte {
	body := concat(
		u32(0), u32(0),
		u32(timescale), u32(duration),
		u16(0x55c4), // language "und"
		u16(0),
	)
	return fullBox("mdhd", 0, 0, body)
}

// hdlr builds a handler box.
func hdlr(handler string) []byte {
	body := concat(
		u32(0), []byte(handler), u32(0), u32(0), u32(0),
		[]byte("MediaHandler\x00"),
	)
	return fullBox("hdlr", 0, 0, body)
}

// elst builds a version-0 edit list.
func elst(entries [][2]int64) []byte {
	body := u32(uint32(len(entries)))
	for _, e := range entries {
		dur, mediaTime := e[0], e[1]
		body = concat(body, u32(uint32(dur)), u32(uint32(int32(mediaTime))), u32(0x00010000), u32(0))
	}
	return fullBox("elst", 0, 0, body)
}

// stts builds a single-run timing table.
func stts(count int, delta uint32) []byte {
	body := concat(u32(1), u32(uint32(count)), u32(delta))
	return fullBox("stts", 0, 0, body)
}

// stsc builds a one-entry sample-to-chunk table.
func stsc(firstChunk, samplesPerChunk, sdi uint32) []byte {
	body := concat(u32(1), u32(firstChunk), u32(samplesPerChunk), u32(sdi))
	return fullBox("stsc", 0, 0, body)
}

// stsz builds a per-sample size table.
func stsz(sizes [][]byte) []byte {
	body := u32(0)
	body = concat(body, u32(uint32(len(sizes))))
	for _, s := range sizes {
		body = concat(body, u32(uint32(len(s))))
	}
	return fullBox("stsz", 0, 0, body)
}

// stco builds a chunk-offset table with one chunk.
func stco(offset int64) []byte {
	body := concat(u32(1), u32(uint32(offset)))
	return fullBox("stco", 0, 0, body)
}

// stss lists sync sample numbers (1-based).
func stss(nums []uint32) []byte {
	body := u32(uint32(len(nums)))
	for _, n := range nums {
		body = concat(body, u32(n))
	}
	return fullBox("stss", 0, 0, body)
}

// ctts builds an all-zero composition offset table (PTS == DTS).
func ctts(count int) []byte {
	body := concat(u32(1), u32(uint32(count)), u32(0))
	return fullBox("ctts", 0, 0, body)
}
