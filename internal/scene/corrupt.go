package scene

import (
	"encoding/binary"
)

// Corruption is a set of damaged inputs plus the fault kind each must trigger.
type Corruption struct {
	// TruncatedBox is an MP4 whose moov box declares more bytes than the file
	// holds; the parser must report a truncated fault at the box offset.
	TruncatedBox []byte
	// TruncatedBoxOffset is the byte offset of the damaged box header.
	TruncatedBoxOffset int64
	// BoundsKLV is an MXF whose KLV length runs past the end of the file.
	BoundsKLV []byte
	// BoundsKLVOffset is the byte offset of the offending KLV key.
	BoundsKLVOffset int64
}

// BuildCorruption derives damaged variants from a valid MP4 container.
func BuildCorruption(mp4 []byte) Corruption {
	return Corruption{
		TruncatedBox:       truncateMoov(mp4),
		TruncatedBoxOffset: findTopLevel(mp4, "moov"),
		BoundsKLV:          oversizeKLV(mp4),
		BoundsKLVOffset:    -1,
	}
}

// findTopLevel returns the offset of the first top-level box with the given
// type, or -1.
func findTopLevel(data []byte, typ string) int64 {
	pos := int64(0)
	for pos+8 <= int64(len(data)) {
		size := int64(binary.BigEndian.Uint32(data[pos : pos+4]))
		if size < 8 {
			return -1
		}
		if string(data[pos+4:pos+8]) == typ {
			return pos
		}
		pos += size
	}
	return -1
}

// truncateMoov copies the file but lies about the moov size, so the box
// extends past the end of the buffer.
func truncateMoov(mp4 []byte) []byte {
	at := findTopLevel(mp4, "moov")
	if at < 0 {
		return nil
	}
	out := append([]byte(nil), mp4...)
	// Claim the file is huge; the parser must notice before allocating.
	binary.BigEndian.PutUint32(out[at:at+4], uint32(len(out)))
	return out[:len(out)-16]
}

// oversizeKLV wraps the MP4 bytes in an MXF header whose first essence KLV
// declares a length far past the end of the file.
func oversizeKLV(mp4 []byte) []byte {
	header := []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x02, 0x04, 0x00}
	essence := []byte{0x06, 0x0e, 0x2b, 0x34, 0x01, 0x02, 0x01, 0x01, 0x0d, 0x01, 0x03, 0x01, 0x15, 0x01, 0x05, 0x00}
	out := make([]byte, 0, 16+2+len(essence)+9)
	out = append(out, header...)
	out = append(out, 0x00) // zero-length partition pack value
	out = append(out, essence...)
	// BER long-form length claiming 2^31 bytes, far beyond the buffer.
	out = append(out, 0x84, 0x7f, 0xff, 0xff, 0xff)
	out = append(out, 0x00, 0x00, 0x00, 0x07) // a few bytes of payload
	return out
}
