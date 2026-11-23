// Package gen builds self-contained CMAF/fMP4 fixtures in memory. It writes
// ISO-BMFF boxes byte-by-byte so the fixtures can be damaged at exact offsets
// and the parser can be tested against known-good and known-bad inputs.
package gen

import (
	"encoding/binary"
)

// u32 appends a big-endian uint32.
func u32(dst []byte, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return append(dst, b[:]...)
}

func u16(dst []byte, v uint16) []byte {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return append(dst, b[:]...)
}

func u64(dst []byte, v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return append(dst, b[:]...)
}

func fourcc(dst []byte, s string) []byte {
	if len(s) != 4 {
		panic("gen: fourcc must be 4 bytes: " + s)
	}
	return append(dst, s...)
}

// box wraps payload with an 8-byte header.
func box(typ string, payload []byte) []byte {
	out := make([]byte, 0, 8+len(payload))
	out = u32(out, uint32(8+len(payload)))
	out = fourcc(out, typ)
	return append(out, payload...)
}

// fullBox wraps payload with version and 24-bit flags.
func fullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	head := make([]byte, 0, 4+len(payload))
	head = append(head, version,
		byte(flags>>16), byte(flags>>8), byte(flags))
	return box(typ, append(head, payload...))
}

// cat concatenates byte slices.
func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// matrix is the identity transformation matrix used by mvhd/tkhd.
func matrix(dst []byte) []byte {
	for _, v := range []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000} {
		dst = u32(dst, v)
	}
	return dst
}

// Ftyp builds a CMAF-compatible file type box.
func Ftyp() []byte {
	var p []byte
	p = fourcc(p, "iso6")
	p = u32(p, 0)
	p = fourcc(p, "iso6")
	p = fourcc(p, "cmfc")
	p = fourcc(p, "mp41")
	return box("ftyp", p)
}

// Styp builds a CMAF segment type box.
func Styp() []byte {
	var p []byte
	p = fourcc(p, "msdh")
	p = u32(p, 0)
	p = fourcc(p, "msdh")
	p = fourcc(p, "msix")
	return box("styp", p)
}

// Mvhd builds a version-0 movie header.
func Mvhd(timescale, duration uint32, nextTrackID uint32) []byte {
	var p []byte
	p = u32(p, 0) // creation
	p = u32(p, 0) // modification
	p = u32(p, timescale)
	p = u32(p, duration)
	p = u32(p, 0x00010000) // rate 1.0
	p = u16(p, 0x0100)     // volume 1.0
	p = u16(p, 0)          // reserved
	p = u32(p, 0)
	p = u32(p, 0)
	p = matrix(p)
	p = u32(p, 0)
	p = u32(p, 0)
	p = u32(p, 0)
	p = u32(p, 0)
	p = u32(p, 0)
	p = u32(p, 0)
	p = u32(p, nextTrackID)
	return fullBox("mvhd", 0, 0, p)
}

// Tkhd builds a version-0 track header for a video track.
func Tkhd(trackID, durationMS, width, height uint32) []byte {
	var p []byte
	p = u32(p, 0)
	p = u32(p, 0)
	p = u32(p, trackID)
	p = u32(p, 0)
	p = u32(p, durationMS)
	p = u32(p, 0)
	p = u32(p, 0)
	p = u16(p, 0) // layer
	p = u16(p, 0) // alternate group
	p = u16(p, 0) // volume (video)
	p = u16(p, 0) // reserved
	p = matrix(p)
	p = u32(p, width<<16)
	p = u32(p, height<<16)
	return fullBox("tkhd", 0, 3, p)
}

// Mdhd builds a version-0 media header.
func Mdhd(timescale, durationTicks uint32) []byte {
	var p []byte
	p = u32(p, 0)
	p = u32(p, 0)
	p = u32(p, timescale)
	p = u32(p, durationTicks)
	p = u16(p, 0x55C4) // "und" packed
	p = u16(p, 0)
	return fullBox("mdhd", 0, 0, p)
}

// Hdlr builds a handler box.
func Hdlr(handler, name string) []byte {
	var p []byte
	p = u32(p, 0)
	p = fourcc(p, handler)
	p = u32(p, 0)
	p = u32(p, 0)
	p = u32(p, 0)
	p = append(p, name...)
	p = append(p, 0)
	return fullBox("hdlr", 0, 0, p)
}

// AvcC builds a minimal AVCDecoderConfigurationRecord.
func AvcC() []byte {
	var p []byte
	p = append(p, 1, 0x64, 0x00, 0x1f)
	p = append(p, 0xff, 0xe1)
	p = u16(p, 4)
	p = append(p, 0x67, 0x64, 0x00, 0x1f)
	p = append(p, 1)
	p = u16(p, 4)
	p = append(p, 0x68, 0xee, 0x3c, 0x80)
	return box("avcC", p)
}

// Avc1 builds a visual sample entry including the avcC child.
func Avc1(width, height uint16) []byte {
	var p []byte
	p = append(p, make([]byte, 6)...) // reserved
	p = u16(p, 1)                     // data reference index
	p = u16(p, 0)                     // pre_defined
	p = u16(p, 0)                     // reserved
	p = u32(p, 0)
	p = u32(p, 0)
	p = u32(p, 0)
	p = u16(p, width)
	p = u16(p, height)
	p = u32(p, 0x00480000)
	p = u32(p, 0x00480000)
	p = u32(p, 0)
	p = u16(p, 1) // frame count
	compressor := make([]byte, 32)
	p = append(p, compressor...)
	p = u16(p, 0x0018) // depth
	p = u16(p, 0xFFFF) // pre_defined
	p = append(p, AvcC()...)
	return box("avc1", p)
}
