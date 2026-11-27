// Package mp4 performs streaming ISO-BMFF (MP4/MOV) container scanning and
// sample-table indexing. Files are read via bounded ReadAt calls so that even
// multi-gigabyte recordings never need to be held in memory.
package mp4

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/fashionweek/mediad/internal/media"
)

// Box is a located ISO-BMFF box. Payload is never retained by the scanner;
// callers read payload bytes themselves through ReaderAt.
type Box struct {
	Type     string
	Offset   int64 // absolute offset of the size field
	Header   int64 // size of the box header (8 or 16)
	Size     int64 // total declared box size
	Payload  int64 // absolute offset of the first payload byte
	Children int64 // payload length available for nested boxes (0 for leafs)
}

// End is the absolute offset immediately after the box.
func (b Box) End() int64 { return b.Offset + b.Size }

// ContentEnd is the offset immediately after the payload; for containers it
// equals Payload+Children, for leafs it equals End().
func (b Box) ContentEnd() int64 {
	if b.Children > 0 {
		return b.Payload + b.Children
	}
	return b.End()
}

// ContainerTypes holds every box whose payload consists of nested boxes.
var ContainerTypes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true,
	"stbl": true, "dinf": true, "edts": true, "mvex": true,
	"moof": true, "traf": true, "mfra": true, "skip": true,
}

// Header is the lightweight result of a top-level scan: it locates the movie
// box and every media-data box without parsing sample tables.
type Header struct {
	FileSize int64
	Moov     Box
	Mdats    []Box
	TopLevel []Box
}

// Scanner walks boxes inside one region of a file.
type Scanner struct {
	ra    io.ReaderAt
	start int64
	end   int64
	pos   int64
}

// NewScanner covers [start,end).
func NewScanner(ra io.ReaderAt, start, end int64) *Scanner {
	return &Scanner{ra: ra, start: start, end: end, pos: start}
}

var errStop = errors.New("scan stop")

// Next returns the next box or io.EOF when the region is exhausted.
func (s *Scanner) Next() (Box, error) {
	if s.pos >= s.end {
		return Box{}, io.EOF
	}
	if s.pos+8 > s.end {
		return Box{}, media.New(media.PhaseScan, media.KindTruncated, "", s.pos,
			"truncated box header: fewer than 8 bytes remain")
	}
	var hdr [8]byte
	if _, err := s.ra.ReadAt(hdr[:], s.pos); err != nil {
		return Box{}, media.New(media.PhaseScan, media.KindTruncated, "", s.pos,
			"cannot read box header").Wrap(err)
	}
	size := int64(binary.BigEndian.Uint32(hdr[0:4]))
	typ := string(hdr[4:8])
	off := s.pos
	box := Box{Offset: off, Header: 8}
	switch size {
	case 1:
		if s.pos+16 > s.end {
			return Box{}, media.New(media.PhaseScan, media.KindTruncated, "", s.pos,
				"truncated largesize box header")
		}
		var ext [8]byte
		if _, err := s.ra.ReadAt(ext[:], s.pos+8); err != nil {
			return Box{}, media.New(media.PhaseScan, media.KindTruncated, "", s.pos,
				"cannot read largesize").Wrap(err)
		}
		size = int64(binary.BigEndian.Uint64(ext[:]))
		box.Header = 16
	case 0:
		size = s.end - s.pos
	}
	if size < box.Header {
		return Box{}, media.New(media.PhaseScan, media.KindBounds, "", off,
			fmt.Sprintf("box %q declares size %d smaller than its header", typ, size))
	}
	box.Type, box.Size = typ, size
	box.Payload = off + box.Header
	box.End()
	if size > s.end-off {
		return Box{}, media.New(media.PhaseScan, media.KindTruncated, "", off,
			fmt.Sprintf("box %q size %d exceeds container end %d", typ, size, s.end))
	}
	if ContainerTypes[typ] {
		box.Children = size - box.Header
	}
	s.pos += size
	return box, nil
}

// ReadPayload loads at most limit bytes of a leaf box payload (0 = no limit).
func ReadPayload(ra io.ReaderAt, b Box, limit int) ([]byte, error) {
	n := b.Size - b.Header
	if limit > 0 && n > int64(limit) {
		return nil, media.New(media.PhaseScan, media.KindBounds, "", b.Payload,
			fmt.Sprintf("box %q payload %d exceeds limit %d", b.Type, n, limit))
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(io.NewSectionReader(ra, b.Payload, n), buf); err != nil {
		return nil, media.New(media.PhaseScan, media.KindTruncated, "", b.Payload,
			"short read of box payload").Wrap(err)
	}
	return buf, nil
}

// Walk visits every box in [start,end), descending into containers.
func Walk(ra io.ReaderAt, start, end int64, visit func(Box) error) error {
	s := NewScanner(ra, start, end)
	for {
		b, err := s.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := visit(b); errors.Is(err, errStop) {
			return nil
		} else if err != nil {
			return err
		}
		if b.Children > 0 {
			if err := Walk(ra, b.Payload, b.Payload+b.Children, visit); err != nil {
				return err
			}
		}
	}
}

// ScanHeader locates moov and all mdat boxes by streaming the top-level atoms
// without descending into container payloads.
func ScanHeader(f *os.File) (*Header, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, media.New(media.PhaseScan, media.KindTool, f.Name(), -1,
			"cannot stat input").Wrap(err)
	}
	h := &Header{FileSize: st.Size()}
	s := NewScanner(f, 0, st.Size())
	for {
		b, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if me, ok := media.As(err); ok && me.Path == "" {
				me.Path = f.Name()
			}
			return nil, err
		}
		h.TopLevel = append(h.TopLevel, b)
		switch b.Type {
		case "moov":
			h.Moov = b
		case "mdat":
			h.Mdats = append(h.Mdats, b)
		}
	}
	if h.Moov.Size == 0 {
		return nil, media.New(media.PhaseScan, media.KindTruncated, f.Name(), st.Size(),
			"moov box missing: unreadable or truncated container")
	}
	if len(h.Mdats) == 0 {
		return nil, media.New(media.PhaseScan, media.KindTruncated, f.Name(), h.Moov.Offset,
			"mdat box missing: no media data present")
	}
	return h, nil
}

// FindBox scans only the direct children of region [start,end) and returns the
// first box of the given type. Nested same-named boxes inside other containers
// are deliberately not matched.
func FindBox(ra io.ReaderAt, start, end int64, typ string) (Box, bool, error) {
	var found Box
	s := NewScanner(ra, start, end)
	for {
		b, err := s.Next()
		if errors.Is(err, io.EOF) {
			return found, found.Size != 0, nil
		}
		if err != nil {
			return Box{}, false, err
		}
		if b.Type == typ {
			return b, true, nil
		}
	}
}

// PayloadReader returns a buffered reader over a box payload.
func PayloadReader(ra io.ReaderAt, b Box) *bufio.Reader {
	return bufio.NewReader(io.NewSectionReader(ra, b.Payload, b.Size-b.Header))
}

// u32/u64 helpers with bounds checks against the remaining payload.
func readU32(buf []byte, at int, boxOff int64) (uint32, int, error) {
	if at+4 > len(buf) {
		return 0, at, media.New(media.PhaseScan, media.KindTruncated, "", boxOff+int64(at),
			"unexpected end of full-box payload")
	}
	return binary.BigEndian.Uint32(buf[at : at+4]), at + 4, nil
}

func readU64(buf []byte, at int, boxOff int64) (uint64, int, error) {
	if at+8 > len(buf) {
		return 0, at, media.New(media.PhaseScan, media.KindTruncated, "", boxOff+int64(at),
			"unexpected end of full-box payload")
	}
	return binary.BigEndian.Uint64(buf[at : at+8]), at + 8, nil
}

func readU16(buf []byte, at int, boxOff int64) (uint16, int, error) {
	if at+2 > len(buf) {
		return 0, at, media.New(media.PhaseScan, media.KindTruncated, "", boxOff+int64(at),
			"unexpected end of full-box payload")
	}
	return binary.BigEndian.Uint16(buf[at : at+2]), at + 2, nil
}

// FullBoxVersion extracts the version/flags prefix of a FullBox.
func FullBoxVersion(b []byte) (version byte, flags uint32, rest []byte) {
	if len(b) < 4 {
		return 0, 0, b
	}
	version = b[0]
	flags = uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	return version, flags, b[4:]
}

// FourCC returns the 4CC inside b at offset at, or "" when the range is out of
// bounds or all four bytes are zero (no codec declared).
func FourCC(b []byte, at int) string {
	if at+4 > len(b) || at < 0 {
		return ""
	}
	raw := b[at : at+4]
	allZero := true
	for _, c := range raw {
		if c != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return ""
	}
	return string(raw)
}
