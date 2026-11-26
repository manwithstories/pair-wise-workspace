// Package bitstream checks PNG/JPEG frame structure and keyframe spacing.
package bitstream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// ErrTruncated marks data that ends before the structure is complete;
// ErrStructure marks data that violates the container format.
var (
	ErrTruncated = errors.New("bitstream: truncated")
	ErrStructure = errors.New("bitstream: bad structure")
)

var pngSig = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

// CheckPNG walks the PNG chunk stream: signature, IHDR first, per-chunk
// CRC, and a terminating IEND with no trailing bytes.
func CheckPNG(data []byte) error {
	if len(data) < 8 {
		return ErrTruncated
	}
	if !bytes.Equal(data[:8], pngSig) {
		return fmt.Errorf("%w: bad PNG signature", ErrStructure)
	}
	off, first := 8, true
	for {
		if off+8 > len(data) {
			return ErrTruncated
		}
		length := int64(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		end := off + int(length) + 12
		if length < 0 || end > len(data) {
			return ErrTruncated
		}
		if first && typ != "IHDR" {
			return fmt.Errorf("%w: first chunk %q is not IHDR", ErrStructure, typ)
		}
		first = false
		want := binary.BigEndian.Uint32(data[end-4:])
		if got := crc32.ChecksumIEEE(data[off+4 : end-4]); got != want {
			return fmt.Errorf("%w: CRC mismatch in chunk %s", ErrStructure, typ)
		}
		off = end
		if typ == "IEND" {
			break
		}
	}
	if off != len(data) {
		return fmt.Errorf("%w: %d trailing bytes after IEND", ErrStructure, len(data)-off)
	}
	return nil
}

// CheckJPEG walks JPEG markers from SOI to EOI, tolerating stuffed 0xFF00
// bytes inside entropy-coded data.
func CheckJPEG(data []byte) error {
	if len(data) < 4 {
		return ErrTruncated
	}
	if data[0] != 0xFF || data[1] != 0xD8 {
		return fmt.Errorf("%w: missing SOI marker", ErrStructure)
	}
	i := 2
	for i < len(data) {
		if data[i] != 0xFF { // entropy-coded byte
			i++
			continue
		}
		for i < len(data) && data[i] == 0xFF { // fill bytes
			i++
		}
		if i >= len(data) {
			return ErrTruncated
		}
		m := data[i]
		i++
		switch {
		case m == 0x00: // stuffed byte, back to entropy data
			continue
		case m == 0xD9: // EOI
			for ; i < len(data); i++ {
				if data[i] != 0x00 && data[i] != 0xFF {
					return fmt.Errorf("%w: trailing bytes after EOI", ErrStructure)
				}
			}
			return nil
		case m == 0xD8 || m == 0x01 || (m >= 0xD0 && m <= 0xD7): // standalone
			continue
		}
		if i+2 > len(data) {
			return ErrTruncated
		}
		l := int(binary.BigEndian.Uint16(data[i:]))
		if l < 2 {
			return fmt.Errorf("%w: bad segment length %d", ErrStructure, l)
		}
		i += l
		if i > len(data) {
			return ErrTruncated
		}
	}
	return ErrTruncated
}

// Check verifies data according to codec ("png" or "jpeg").
func Check(codec string, data []byte) error {
	switch codec {
	case "png":
		return CheckPNG(data)
	case "jpeg":
		return CheckJPEG(data)
	default:
		return fmt.Errorf("%w: unknown codec %q", ErrStructure, codec)
	}
}

// KeyframeViolations returns the keyframe indices whose distance from the
// previous keyframe exceeds maxGap frames. The first frame must be a
// keyframe; otherwise index 0 is reported.
func KeyframeViolations(key []bool, maxGap int) []int {
	var out []int
	if len(key) == 0 {
		return nil
	}
	if !key[0] {
		out = append(out, 0)
	}
	last := 0
	for i := 1; i < len(key); i++ {
		if key[i] {
			if i-last > maxGap {
				out = append(out, i)
			}
			last = i
		}
	}
	return out
}
