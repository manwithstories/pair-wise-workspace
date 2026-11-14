// Package box provides bounded, read-only parsing of ISO BMFF box structure.
package box

import (
	"encoding/binary"
	"errors"
	"io"
)

// MaxDepth limits container nesting to guard against malicious files.
const MaxDepth = 16

var (
	ErrOutOfRange = errors.New("box_out_of_range")
	ErrTooDeep    = errors.New("box_nesting_too_deep")
	ErrBadSize    = errors.New("box_bad_size")
)

// Header describes one box located at Offset within its parent extent.
type Header struct {
	Offset     uint64 // absolute file offset of the box
	Size       uint64 // total box size including header
	Type       string
	HeaderSize uint64
}

// DataOffset returns the absolute offset of the box payload.
func (h Header) DataOffset() uint64 { return h.Offset + h.HeaderSize }

// DataSize returns the payload size.
func (h Header) DataSize() uint64 { return h.Size - h.HeaderSize }

// End returns the absolute offset one past the box.
func (h Header) End() uint64 { return h.Offset + h.Size }

// ReadHeader reads a box header at off, bounded by parentEnd (exclusive).
// Handles 32-bit size, size==1 (64-bit largesize), size==0 (to end of
// parent) and uuid boxes (16-byte usertype extends the header).
func ReadHeader(r io.ReaderAt, off, parentEnd uint64) (Header, error) {
	if off+8 > parentEnd {
		return Header{}, ErrOutOfRange
	}
	var buf [16]byte
	if _, err := r.ReadAt(buf[:8], int64(off)); err != nil {
		return Header{}, err
	}
	size := uint64(binary.BigEndian.Uint32(buf[0:4]))
	typ := string(buf[4:8])
	hs := uint64(8)
	switch size {
	case 0:
		size = parentEnd - off
	case 1:
		if off+16 > parentEnd {
			return Header{}, ErrOutOfRange
		}
		if _, err := r.ReadAt(buf[8:16], int64(off+8)); err != nil {
			return Header{}, err
		}
		size = binary.BigEndian.Uint64(buf[8:16])
		hs = 16
	}
	if typ == "uuid" {
		hs += 16
	}
	if size < hs {
		return Header{}, ErrBadSize
	}
	if size > parentEnd-off {
		return Header{}, ErrOutOfRange
	}
	return Header{Offset: off, Size: size, Type: typ, HeaderSize: hs}, nil
}

// containers are box types whose payload is a sequence of child boxes.
var containers = map[string]bool{
	"moov": true, "trak": true, "edts": true, "mdia": true,
	"minf": true, "dinf": true, "stbl": true, "dref": true, "stsd": true,
}

// IsContainer reports whether the box type nests child boxes.
func IsContainer(typ string) bool { return containers[typ] }

// ChildStart returns the payload offset where child boxes begin.
// stsd and dref carry an 8-byte version/flags + entry-count prefix.
func ChildStart(h Header) uint64 {
	off := h.DataOffset()
	if h.Type == "stsd" || h.Type == "dref" {
		off += 8
	}
	return off
}

// Walk iterates top-level boxes in [start, end), invoking fn for each.
func Walk(r io.ReaderAt, start, end uint64, fn func(Header) error) error {
	for off := start; off < end; {
		h, err := ReadHeader(r, off, end)
		if err != nil {
			return err
		}
		if err := fn(h); err != nil {
			return err
		}
		off = h.End()
	}
	return nil
}

// CountBoxes counts boxes recursively, bounded by MaxDepth.
func CountBoxes(r io.ReaderAt, start, end uint64, depth int) (int, error) {
	if depth > MaxDepth {
		return 0, ErrTooDeep
	}
	n := 0
	err := Walk(r, start, end, func(h Header) error {
		n++
		if IsContainer(h.Type) {
			c, err := CountBoxes(r, ChildStart(h), h.End(), depth+1)
			if err != nil {
				return err
			}
			n += c
		}
		return nil
	})
	return n, err
}
