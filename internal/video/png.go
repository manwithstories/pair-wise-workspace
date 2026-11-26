package video

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image/png"

	"pairflow/internal/media"
)

// ValidatePNG independently parses a PNG byte stream, verifies every chunk
// CRC, confirms the required color chunks and decodes the pixels. A frame that
// fails this check may never be committed.
func ValidatePNG(data []byte, path string) *media.Error {
	if len(data) < 8 || !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return media.Integrity("video.png_signature", "missing PNG signature").WithPath(path)
	}
	off := 8
	sawIEND := false
	sawSRGB := false
	for off+8 <= len(data) {
		n := int(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		end := off + 12 + n
		if end > len(data) {
			return media.Integrity("video.png_truncated", "chunk %q overruns the stream", typ).WithPath(path)
		}
		want := binary.BigEndian.Uint32(data[end-4 : end])
		sum := crc32.NewIEEE()
		_, _ = sum.Write(data[off+4 : end-4])
		if got := sum.Sum32(); got != want {
			return media.Integrity("video.png_crc", "chunk %q crc %08x, want %08x", typ, got, want).WithPath(path)
		}
		switch typ {
		case "sRGB":
			sawSRGB = true
		case "IEND":
			sawIEND = true
		}
		off = end
		if typ == "IEND" {
			break
		}
	}
	if !sawIEND {
		return media.Integrity("video.png_no_iend", "PNG is missing IEND").WithPath(path)
	}
	if !sawSRGB {
		return media.Integrity("video.png_no_srgb", "PNG is missing the sRGB chunk").WithPath(path)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		return media.Integrity("video.png_decode", "PNG does not decode: %v", err).WithPath(path)
	}
	return nil
}

// ImageSHA is the stable identity of a PNG artifact. Callers compare it with
// the recorded per-frame hash.
func ImageSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}
