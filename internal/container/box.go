// Low-level MP4 box helpers: header parsing, big-endian decoding and
// bounded byte access. Kept separate from the tree walker so that the
// offset-attribution rules for malformed headers live in one place.
package container

import (
	"errors"
	"fmt"
	"io"
	"os"

	"mediacore/internal/media"
)

// MaxBoxBytes bounds a single declared box size. Anything larger is treated
// as an oversized declaration rather than an allocation request.
const MaxBoxBytes = int64(512) << 20

// boxHeader is the result of parsing one box header at a known offset.
type boxHeader struct {
	start      int64
	end        int64 // start + declared size
	payload    int64 // start + header size
	typ        string
	size       int64
	headerSize int64
}

var errShortHeader = errors.New("short box header")

// readBoxHeader parses the common header at off without trusting its size.
// A size that runs past fileSize is rejected here, before any allocation:
// a truncated mdat is attributed to its payload start, every other overlong
// box to the offset of its size field.
func readBoxHeader(r io.ReaderAt, off, fileSize int64) (boxHeader, error) {
	if fileSize-off < 8 {
		if fileSize-off == 0 {
			return boxHeader{}, io.EOF
		}
		return boxHeader{}, media.NewError(media.CodeInvalidBoxLength, off, 0,
			"only %d trailing bytes, need 8 for a box header", fileSize-off)
	}
	var hdr [16]byte
	n := int64(8)
	if fileSize-off < 16 {
		n = fileSize - off
	}
	if _, err := r.ReadAt(hdr[:n], off); err != nil && err != io.EOF {
		return boxHeader{}, media.NewError(media.CodeInvalidBoxLength, off, 0, "read box header: %v", err).Wrap(err)
	}
	size := int64(be32(hdr[0:4]))
	typ := string(hdr[4:8])
	headerSize := int64(8)
	switch size {
	case 0:
		size = fileSize - off
	case 1:
		if fileSize-off < 16 {
			return boxHeader{}, media.NewError(media.CodeInvalidBoxLength, off, 0,
				"64-bit box header truncated: %d bytes available", fileSize-off)
		}
		if _, err := r.ReadAt(hdr[:16], off); err != nil {
			return boxHeader{}, media.NewError(media.CodeInvalidBoxLength, off, 0, "read largesize: %v", err).Wrap(err)
		}
		size = int64(be64(hdr[8:16]))
		headerSize = 16
	default:
		if size < 8 {
			return boxHeader{}, media.NewError(media.CodeInvalidBoxLength, off, 0,
				"box %q declares size %d, smaller than its header", typ, size)
		}
	}
	if size > MaxBoxBytes {
		return boxHeader{}, media.NewError(media.CodeOversizedBox, off, 0,
			"box %q declares %d bytes, above limit %d", typ, size, MaxBoxBytes)
	}
	end := off + size
	if end > fileSize {
		if typ == "mdat" {
			return boxHeader{}, media.NewError(media.CodeTruncatedMdat, off+headerSize, 0,
				"mdat declares %d bytes but only %d remain", size, fileSize-off)
		}
		return boxHeader{}, media.NewError(media.CodeBoxBeyondEOF, off, 0,
			"box %q declares size %d, file has %d bytes left", typ, size, fileSize-off)
	}
	return boxHeader{start: off, end: end, payload: off + headerSize, typ: typ, size: size, headerSize: headerSize}, nil
}

// readPayload reads a box body into memory. Bounds were validated by
// readBoxHeader, so the only failure left is an underlying I/O error.
func readPayload(r io.ReaderAt, bi boxHeader) ([]byte, error) {
	body := make([]byte, bi.size-bi.headerSize)
	if _, err := r.ReadAt(body, bi.payload); err != nil {
		return nil, media.NewError(media.CodeBoxBeyondEOF, bi.start, 0, "read %s payload: %v", bi.typ, err).Wrap(err)
	}
	return body, nil
}

// children yields the headers of the boxes inside [start, end).
func children(r io.ReaderAt, start, end int64, fn func(boxHeader) error) error {
	for off := start; off < end; {
		child, err := readBoxHeader(r, off, end)
		if err != nil {
			return err
		}
		if err := fn(child); err != nil {
			return err
		}
		if child.end <= off {
			return media.NewError(media.CodeInvalidBoxLength, off, 0, "box %q does not advance", child.typ)
		}
		off = child.end
	}
	return nil
}

// sliceReader adapts a byte slice to io.ReadSeeker for go-mp4's Unmarshal.
func bytesReader(b []byte) io.ReadSeeker { return &sliceReader{b: b} }

type sliceReader struct {
	b   []byte
	pos int64
}

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.pos >= int64(len(s.b)) {
		return 0, io.EOF
	}
	n := copy(p, s.b[s.pos:])
	s.pos += int64(n)
	return n, nil
}

func (s *sliceReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = s.pos + offset
	case io.SeekEnd:
		abs = int64(len(s.b)) + offset
	default:
		return 0, errors.New("bad whence")
	}
	if abs < 0 || abs > int64(len(s.b)) {
		return 0, errors.New("seek out of range")
	}
	s.pos = abs
	return abs, nil
}

func sizeOf(r io.ReaderAt) (int64, error) {
	if s, ok := r.(interface{ Stat() (os.FileInfo, error) }); ok {
		fi, err := s.Stat()
		if err != nil {
			return 0, err
		}
		return fi.Size(), nil
	}
	if s, ok := r.(interface{ Size() int64 }); ok {
		return s.Size(), nil
	}
	return 0, fmt.Errorf("reader has no size")
}

func be16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }
func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
func be64(b []byte) uint64 {
	return uint64(be32(b[0:4]))<<32 | uint64(be32(b[4:8]))
}
