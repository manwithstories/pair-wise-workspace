package meta

import (
	"encoding/binary"
)

// MXFCases is the deterministic MXF fixture catalog.
var MXFCases = []string{
	"ok", "illegal-rotation", "bad-crop", "unknown-color",
	"color-conflict", "truncated-mxf", "unknown-tag",
}

func berLength(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	var be [8]byte
	binary.BigEndian.PutUint64(be[:], uint64(n))
	k := 0
	for k < 7 && be[k] == 0 {
		k++
	}
	out := []byte{0x80 | byte(8-k)}
	return append(out, be[k:]...)
}

func klv(tag byte, val []byte) []byte {
	k := mxfKey(tag)
	out := append([]byte{}, k[:]...)
	out = append(out, berLength(len(val))...)
	return append(out, val...)
}

func u32v(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u64v(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func i32v(v int32) []byte { return u32v(uint32(v)) }

func mxfColorValue(prim, trc, mat uint16, full, known bool) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint16(b[0:2], prim)
	binary.BigEndian.PutUint16(b[2:4], trc)
	binary.BigEndian.PutUint16(b[4:6], mat)
	if full {
		b[6] = 1
	}
	if known {
		b[7] = 1
	}
	return b
}

func mxfCropValue(x, y, w, h, fw, fh int64) []byte {
	b := make([]byte, 48)
	put := func(at int, v int64) { binary.BigEndian.PutUint64(b[at:at+8], uint64(v)) }
	put(0, x)
	put(8, y)
	put(16, w)
	put(24, h)
	put(32, fw)
	put(40, fh)
	return b
}

// BuildMXFCase returns deterministic convention-subset KLV bytes.
func BuildMXFCase(name string) []byte {
	var out []byte
	out = append(out, klv(mxfTagTrackID, u32v(1))...)
	out = append(out, klv(mxfTagTimeScale, u32v(48000))...)
	out = append(out, klv(mxfTagDuration, u64v(192000))...)

	bt709 := mxfColorValue(1, 1, 1, false, true)
	bt2020 := mxfColorValue(9, 16, 9, true, true)
	goodCrop := mxfCropValue(0, 0, 1920, 1080, 1920, 1080)
	badCrop := mxfCropValue(0, 0, 5000, 1080, 1920, 1080)

	switch name {
	case "illegal-rotation":
		out = append(out, klv(mxfTagRotation, i32v(45))...)
		out = append(out, klv(mxfTagColor, bt709)...)
	case "bad-crop":
		out = append(out, klv(mxfTagRotation, i32v(90))...)
		out = append(out, klv(mxfTagCrop, badCrop)...)
		out = append(out, klv(mxfTagColor, bt709)...)
	case "unknown-color":
		out = append(out, klv(mxfTagColor, mxfColorValue(60000, 60001, 60002, false, false))...)
	case "color-conflict":
		out = append(out, klv(mxfTagColor, bt2020)...)
		out = append(out, klv(mxfTagContainerColor, bt709)...)
	default: // "ok"
		out = append(out, klv(mxfTagRotation, i32v(90))...)
		out = append(out, klv(mxfTagCrop, goodCrop)...)
		out = append(out, klv(mxfTagColor, bt2020)...)
		out = append(out, klv(mxfTagContainerColor, bt709)...)
	}

	if name == "unknown-tag" {
		out = append(out, klv(0x77, []byte("preserve-me"))...)
	}
	if name == "truncated-mxf" {
		k := mxfKey(0x12)
		out = append(out, k[:]...)
		out = append(out, 0x84) // BER long form: 4 length bytes
		lb := make([]byte, 4)
		binary.BigEndian.PutUint32(lb, 1_000_000)
		out = append(out, lb...)
		out = append(out, 0, 1) // far fewer than claimed
	}
	return out
}
