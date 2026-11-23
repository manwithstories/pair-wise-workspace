// Package parse implements streaming ISO-BMFF box parsing with precise,
// offset-bearing faults. It deliberately validates every length before
// allocating or reading so malformed archives cannot exhaust memory.
package parse

import (
	"errors"
	"fmt"
	"io"
	"os"

	"archivecheck/internal/fault"
)

// MaxBoxPayload bounds any box payload copied into memory. Sample payloads in
// mdat are never copied; this limit applies to structural boxes (moov, moof).
const MaxBoxPayload = 8 << 20

// Header describes a box header at a concrete file offset.
type Header struct {
	Type         string
	Offset       int64
	Size         uint64
	HeaderSize   uint64
	ExtendsToEOF bool
}

// PayloadOffset returns the absolute offset of the first payload byte.
func (h Header) PayloadOffset() int64 { return h.Offset + int64(h.HeaderSize) }

// PayloadSize returns the number of payload bytes.
func (h Header) PayloadSize() uint64 { return h.Size - h.HeaderSize }

// End returns the absolute offset one past the box.
func (h Header) End() int64 { return h.Offset + int64(h.Size) }

// PrintableType renders a four-character box type, hex-encoding non-printable
// bytes so diagnostics never emit control characters.
func PrintableType(b []byte) string {
	if len(b) != 4 {
		return fmt.Sprintf("%x", b)
	}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return fmt.Sprintf("0x%02x%02x%02x%02x", b[0], b[1], b[2], b[3])
		}
	}
	return string(b)
}

// ReadHeaderAt reads a box header from r's current position and validates it
// against fileSize. All failures carry the header's raw start offset.
func ReadHeaderAt(r io.ReadSeeker, fileSize int64, segment string) (Header, error) {
	start, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return Header{}, fault.Wrap(err, fault.KindShortRead, segment, "", 0, "seek failed")
	}
	if start < 0 || start > fileSize {
		return Header{}, fault.New(fault.KindOutOfBounds, segment, "", start, "offset is outside the input file")
	}
	var head [8]byte
	n, err := io.ReadFull(r, head[:])
	if err != nil {
		kind := fault.KindShortRead
		if errors.Is(err, io.EOF) && n == 0 {
			kind = fault.KindShortRead
		}
		return Header{}, fault.Wrap(err, kind, segment, "", start,
			fmt.Sprintf("truncated box header: got %d of 8 bytes", n))
	}
	size32 := uint64(head[0])<<24 | uint64(head[1])<<16 | uint64(head[2])<<8 | uint64(head[3])
	typ := PrintableType(head[4:8])
	h := Header{Type: typ, Offset: start, Size: size32, HeaderSize: 8}
	switch size32 {
	case 0:
		h.Size = uint64(fileSize - start)
		h.ExtendsToEOF = true
	case 1:
		var ext [8]byte
		n, err := io.ReadFull(r, ext[:])
		if err != nil {
			return Header{}, fault.Wrap(err, fault.KindShortRead, segment, typ, start,
				fmt.Sprintf("truncated extended size: got %d of 8 bytes", n))
		}
		h.Size = uint64(ext[0])<<56 | uint64(ext[1])<<48 | uint64(ext[2])<<40 | uint64(ext[3])<<32 |
			uint64(ext[4])<<24 | uint64(ext[5])<<16 | uint64(ext[6])<<8 | uint64(ext[7])
		h.HeaderSize = 16
	}
	if h.Size < h.HeaderSize {
		return Header{}, fault.New(fault.KindInvalidSize, segment, typ, start,
			fmt.Sprintf("box size %d is smaller than its %d-byte header", h.Size, h.HeaderSize))
	}
	remaining := uint64(fileSize - start)
	if h.Size > remaining {
		return Header{}, fault.New(fault.KindOutOfBounds, segment, typ, start,
			fmt.Sprintf("box size %d exceeds remaining %d bytes", h.Size, remaining))
	}
	return h, nil
}

// WalkFile streams the top-level boxes of f. The callback may read the
// payload; the walker always repositions to the next box afterwards.
func WalkFile(f *os.File, segment string, fn func(Header, io.ReadSeeker) error) error {
	info, err := f.Stat()
	if err != nil {
		return fault.Wrap(err, fault.KindShortRead, segment, "", 0, "stat failed")
	}
	return WalkReader(f, info.Size(), segment, fn)
}

// WalkReader streams top-level boxes from an arbitrary reader of known size.
func WalkReader(r io.ReadSeeker, fileSize int64, segment string, fn func(Header, io.ReadSeeker) error) error {
	f := r
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fault.Wrap(err, fault.KindShortRead, segment, "", 0, "seek failed")
	}
	for {
		pos, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return fault.Wrap(err, fault.KindShortRead, segment, "", 0, "seek failed")
		}
		if pos == fileSize {
			return nil
		}
		h, err := ReadHeaderAt(f, fileSize, segment)
		if err != nil {
			return err
		}
		if err := fn(h, f); err != nil {
			return err
		}
		if _, err := f.Seek(h.End(), io.SeekStart); err != nil {
			return fault.Wrap(err, fault.KindShortRead, segment, h.Type, h.Offset, "seek to box end failed")
		}
	}
}

// ReadPayload copies and returns a bounded payload.
func ReadPayload(r io.ReadSeeker, h Header, segment string) ([]byte, error) {
	size := h.PayloadSize()
	if size > MaxBoxPayload {
		return nil, fault.New(fault.KindInvalidSize, segment, h.Type, h.Offset,
			fmt.Sprintf("payload %d exceeds limit %d", size, MaxBoxPayload))
	}
	if _, err := r.Seek(h.PayloadOffset(), io.SeekStart); err != nil {
		return nil, fault.Wrap(err, fault.KindShortRead, segment, h.Type, h.Offset, "seek to payload failed")
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fault.Wrap(err, fault.KindShortRead, segment, h.Type, h.PayloadOffset(),
			fmt.Sprintf("short payload read of %d bytes", size))
	}
	return buf, nil
}

// WalkPayload walks boxes contained in data whose first byte is at baseOffset
// in the segment. It applies the same length checks as top-level scanning.
func WalkPayload(data []byte, baseOffset int64, segment string, fn func(Header, []byte) error) error {
	pos := 0
	for pos < len(data) {
		remaining := len(data) - pos
		if remaining < 8 {
			return fault.New(fault.KindShortRead, segment, "", baseOffset+int64(pos),
				fmt.Sprintf("truncated child box header: %d bytes left", remaining))
		}
		size32 := uint64(data[pos])<<24 | uint64(data[pos+1])<<16 | uint64(data[pos+2])<<8 | uint64(data[pos+3])
		typ := PrintableType(data[pos+4 : pos+8])
		h := Header{Type: typ, Offset: baseOffset + int64(pos), Size: size32, HeaderSize: 8}
		switch size32 {
		case 0:
			h.Size = uint64(remaining)
			h.ExtendsToEOF = true
		case 1:
			if remaining < 16 {
				return fault.New(fault.KindShortRead, segment, typ, h.Offset, "truncated extended size")
			}
			h.Size = uint64(data[pos+8])<<56 | uint64(data[pos+9])<<48 | uint64(data[pos+10])<<40 |
				uint64(data[pos+11])<<32 | uint64(data[pos+12])<<24 | uint64(data[pos+13])<<16 |
				uint64(data[pos+14])<<8 | uint64(data[pos+15])
			h.HeaderSize = 16
		}
		if h.Size < h.HeaderSize {
			return fault.New(fault.KindInvalidSize, segment, typ, h.Offset,
				fmt.Sprintf("child box size %d is smaller than its header", h.Size))
		}
		if h.Size > uint64(remaining) {
			return fault.New(fault.KindOutOfBounds, segment, typ, h.Offset,
				fmt.Sprintf("child box size %d exceeds remaining %d bytes", h.Size, remaining))
		}
		payload := data[pos+int(h.HeaderSize) : pos+int(h.Size)]
		if err := fn(h, payload); err != nil {
			return err
		}
		pos += int(h.Size)
	}
	return nil
}
