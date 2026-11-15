package box

import (
	"encoding/binary"
	"io"
)

// ReadHeader parses one box header at off. parentEnd bounds the box: a
// size of 0 extends the box to parentEnd (per spec, to end of file for
// top-level boxes). 32-bit, 64-bit (largesize) and uuid headers are
// supported.
func ReadHeader(r io.ReaderAt, off, parentEnd uint64) (Header, error) {
	if off+SmallHeaderSize > parentEnd {
		return Header{}, ErrTruncated("read header", off, SmallHeaderSize, parentEnd-off)
	}
	var buf [LargeHeaderSize]byte
	if _, err := r.ReadAt(buf[:SmallHeaderSize], int64(off)); err != nil {
		return Header{}, ErrTruncated("read header", off, SmallHeaderSize, parentEnd-off)
	}
	size32 := binary.BigEndian.Uint32(buf[0:4])
	typ := string(buf[4:8])
	h := Header{Type: typ, Offset: off, HeaderSize: SmallHeaderSize}
	switch size32 {
	case 0:
		// Box extends to the end of the enclosing region.
		h.Size = parentEnd - off
	case 1:
		if off+LargeHeaderSize > parentEnd {
			return Header{}, ErrTruncated("read largesize", off, LargeHeaderSize, parentEnd-off)
		}
		if _, err := r.ReadAt(buf[:LargeHeaderSize], int64(off)); err != nil {
			return Header{}, ErrTruncated("read largesize", off, LargeHeaderSize, parentEnd-off)
		}
		h.Size = binary.BigEndian.Uint64(buf[8:16])
		h.HeaderSize = LargeHeaderSize
	default:
		h.Size = uint64(size32)
	}
	if typ == TypeUUID {
		h.HeaderSize += UUIDExtra
	}
	if h.Size < h.HeaderSize {
		return Header{}, errf("validate header", off, "box %q size %d smaller than header %d", typ, h.Size, h.HeaderSize)
	}
	if h.Size > parentEnd-off {
		return Header{}, ErrTruncated("validate header", off, h.Size, parentEnd-off)
	}
	return h, nil
}

// WalkFunc is invoked for every direct child of a walked region. Return
// true to recurse into the box (only honoured for known containers).
type WalkFunc func(h Header) (recurse bool, err error)

// Walker traverses box hierarchies with a depth limit and counts skipped
// unknown boxes.
type Walker struct {
	// Unknown counts boxes whose fourcc is not recognised; they are
	// skipped without descending.
	Unknown int
}

// Walk iterates the children of [start, end) at the given depth (top
// level is 0) and invokes fn for each. Container boxes are recursed into
// when fn returns true, up to MaxDepth.
func (w *Walker) Walk(r io.ReaderAt, start, end uint64, depth int, fn WalkFunc) error {
	if depth > MaxDepth {
		return errf("walk", start, "nesting depth exceeds limit %d", MaxDepth)
	}
	off := start
	for off < end {
		h, err := ReadHeader(r, off, end)
		if err != nil {
			return err
		}
		if !IsKnown(h.Type) {
			w.Unknown++
		}
		recurse, err := fn(h)
		if err != nil {
			return err
		}
		if recurse && IsContainer(h.Type) {
			if err := w.Walk(r, h.DataOffset(), h.End(), depth+1, fn); err != nil {
				return err
			}
		}
		off = h.End()
	}
	if off != end {
		return errf("walk", off, "region overrun: stopped at %d, region ends %d", off, end)
	}
	return nil
}

// Children returns the payload range of h for use as Walk arguments.
func Children(h Header) (start, end uint64) { return h.DataOffset(), h.End() }

// FindChild returns the first direct child of [start, end) with the
// given fourcc.
func FindChild(r io.ReaderAt, start, end uint64, typ string) (Header, bool, error) {
	var found Header
	var ok bool
	w := &Walker{}
	err := w.Walk(r, start, end, 0, func(h Header) (bool, error) {
		if h.Type == typ {
			found, ok = h, true
		}
		return false, nil
	})
	return found, ok, err
}

// ReadPayload loads the payload of h into memory, guarding against
// oversized tables.
func ReadPayload(r io.ReaderAt, h Header) ([]byte, error) {
	if h.Payload() > MaxPayload {
		return nil, errf("read payload", h.Offset, "box %q payload %d exceeds limit %d", h.Type, h.Payload(), MaxPayload)
	}
	buf := make([]byte, h.Payload())
	if _, err := r.ReadAt(buf, int64(h.DataOffset())); err != nil {
		return nil, ErrTruncated("read payload", h.DataOffset(), h.Payload(), h.Payload())
	}
	return buf, nil
}
