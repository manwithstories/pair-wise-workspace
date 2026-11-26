package bmff

import (
	"encoding/binary"
	"io"
	"math"

	"mediad/internal/model"
)

const DefaultMaxBoxDepth = 40

type Reader struct {
	R          io.ReaderAt
	Size       int64
	Source     string
	MaxDepth   int
	MaxSamples int
}

func NewReader(source string, r io.ReaderAt, size int64) *Reader {
	return &Reader{R: r, Size: size, Source: source, MaxDepth: DefaultMaxBoxDepth}
}

func (s *Reader) readAt(p []byte, off int64) error {
	if off < 0 || off > s.Size {
		return io.ErrUnexpectedEOF
	}
	n, err := s.R.ReadAt(p, off)
	if err == io.EOF && n == len(p) {
		return nil
	}
	if err != nil && err != io.EOF {
		return err
	}
	if n != len(p) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// ReadBoxAt parses one box header and validates its complete extent against
// end. Every malformed length carries the exact header offset.
func (s *Reader) ReadBoxAt(off, end int64, depth int) (*BoxHeader, error) {
	if depth > s.MaxDepth {
		return nil, model.New(model.CodeUnsupported, model.StageScan, "box nesting depth limit exceeded").
			WithSource(s.Source).WithOffset(off)
	}
	if off == end {
		return nil, io.EOF
	}
	var raw [16]byte
	if err := s.readAt(raw[:8], off); err != nil {
		e := model.New(model.CodeCorruptBox, model.StageScan, "truncated box header").
			WithSource(s.Source).WithExpectedOffset(off)
		return nil, e
	}
	size32 := binary.BigEndian.Uint32(raw[0:4])
	typ := string(raw[4:8])
	b := &BoxHeader{Type: typ, Depth: depth, Offset: off, HeaderSize: 8}

	size := int64(size32)
	if size32 == 1 {
		if err := s.readAt(raw[8:16], off+8); err != nil {
			return nil, corruptBox(s.Source, b, "truncated 64-bit box size")
		}
		large := binary.BigEndian.Uint64(raw[8:16])
		if large > math.MaxInt64 {
			return nil, corruptBox(s.Source, b, "64-bit box size overflows int64")
		}
		size = int64(large)
		b.HeaderSize = 16
	} else if size32 == 0 {
		size = end - off
	}
	b.Size = size
	b.PayloadOffset = off + b.HeaderSize
	b.End = off + size

	if size < b.HeaderSize {
		return nil, corruptBox(s.Source, b, "box size %d smaller than header %d", size, b.HeaderSize)
	}
	if end > 0 && b.End > end {
		return nil, corruptBox(s.Source, b, "box end %d exceeds parent/file end %d", b.End, end)
	}
	if b.End < off {
		return nil, corruptBox(s.Source, b, "box end overflow")
	}
	return b, nil
}

// Children lists direct child boxes. Unknown boxes are skipped structurally;
// the parent Depth drives the enforced nesting limit.
func (s *Reader) Children(parent *BoxHeader) ([]BoxHeader, error) {
	if parent == nil {
		return s.Range(0, s.Size, 1)
	}
	return s.Range(parent.PayloadOffset, parent.End, parent.Depth+1)
}

func (s *Reader) Range(start, end int64, depth int) ([]BoxHeader, error) {
	var out []BoxHeader
	off := start
	for off < end {
		b, err := s.ReadBoxAt(off, end, depth)
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
		out = append(out, *b)
		if b.Size <= 0 {
			return out, corruptBox(s.Source, b, "non-positive box size")
		}
		off = b.End
	}
	if off != end {
		return out, model.Newf(model.CodeCorruptBox, model.StageScan, "box range ends at %d, expected %d", off, end).
			WithSource(s.Source).WithDetectedOffset(end, off)
	}
	return out, nil
}

func (s *Reader) FirstChild(parent *BoxHeader, typ string) (*BoxHeader, error) {
	children, err := s.Children(parent)
	if err != nil {
		return nil, err
	}
	for i := range children {
		if children[i].Type == typ {
			return &children[i], nil
		}
	}
	return nil, nil
}

func (s *Reader) MustChild(parent *BoxHeader, typ string) (*BoxHeader, error) {
	b, err := s.FirstChild(parent, typ)
	if err != nil {
		return nil, err
	}
	if b == nil {
		where := "root"
		if parent != nil {
			where = parent.Type
		}
		return nil, model.Newf(model.CodeCorruptBox, model.StageScan, "missing %s child in %s", typ, where).
			WithSource(s.Source).WithBox(typ)
	}
	return b, nil
}

// FindPath follows exact box types from the root.
func (s *Reader) FindPath(path ...string) (*BoxHeader, error) {
	var cur *BoxHeader
	for _, typ := range path {
		next, err := s.FirstChild(cur, typ)
		if err != nil {
			return nil, err
		}
		if next == nil {
			return nil, nil
		}
		cur = next
	}
	return cur, nil
}

func (s *Reader) ReadFull(b *BoxHeader) ([]byte, error) {
	buf := make([]byte, b.Size)
	if err := s.readAt(buf, b.Offset); err != nil {
		return nil, corruptBox(s.Source, b, "read box failed: %v", err)
	}
	return buf, nil
}

func (s *Reader) ReadPayload(b *BoxHeader) ([]byte, error) {
	buf := make([]byte, b.Size-b.HeaderSize)
	if err := s.readAt(buf, b.PayloadOffset); err != nil {
		return nil, corruptBox(s.Source, b, "read payload failed: %v", err)
	}
	return buf, nil
}

func (s *Reader) ReadAtSize(off, n int64) ([]byte, error) {
	buf := make([]byte, n)
	if err := s.readAt(buf, off); err != nil {
		return nil, model.Newf(model.CodeCorruptBox, model.StageScan, "read %d bytes failed", n).
			WithSource(s.Source).WithOffset(off)
	}
	return buf, nil
}

// FullBoxVersionFlags reads the one-byte version and three-byte flags.
func FullBoxVersionFlags(payload []byte) (uint8, uint32, error) {
	if len(payload) < 4 {
		return 0, 0, io.ErrUnexpectedEOF
	}
	return payload[0], uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3]), nil
}
