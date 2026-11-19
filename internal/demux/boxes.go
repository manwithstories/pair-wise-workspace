package demux

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	mp4 "github.com/abema/go-mp4"
)

// maxMoovBytes bounds the metadata buffer so a hostile or damaged size
// field cannot exhaust memory; sample payloads are always read on demand.
const maxMoovBytes = 64 << 20

type boxHeader struct {
	typ     string
	off     int64 // offset of the box header
	size    uint64
	hdrSize int64
}

func (b boxHeader) payloadOff() int64  { return b.off + b.hdrSize }
func (b boxHeader) payloadLen() uint64 { return b.size - uint64(b.hdrSize) }

type errMissing string

func (e errMissing) Error() string { return "missing box: " + string(e) }

// readBoxHeader reads one box header at off; end bounds the parent box.
func readBoxHeader(r io.ReaderAt, off, end int64) (boxHeader, error) {
	var hdr [8]byte
	if _, err := r.ReadAt(hdr[:], off); err != nil {
		return boxHeader{}, err
	}
	size := uint64(binary.BigEndian.Uint32(hdr[0:4]))
	h := boxHeader{typ: string(hdr[4:8]), off: off, hdrSize: 8}
	switch size {
	case 0: // extends to end of enclosing box
		size = uint64(end - off)
	case 1:
		var ext [8]byte
		if _, err := r.ReadAt(ext[:], off+8); err != nil {
			return boxHeader{}, err
		}
		size = binary.BigEndian.Uint64(ext[:])
		h.hdrSize = 16
	}
	if size < uint64(h.hdrSize) || off+int64(size) > end {
		return boxHeader{}, fmt.Errorf("box %q at %d size %d exceeds bounds [%d,%d)",
			h.typ, off, size, off, end)
	}
	h.size = size
	return h, nil
}

// walkBoxes iterates the direct children between [start,end).
func walkBoxes(r io.ReaderAt, start, end int64, fn func(boxHeader) error) error {
	for off := start; off < end; {
		h, err := readBoxHeader(r, off, end)
		if err != nil {
			return err
		}
		if err := fn(h); err != nil {
			return err
		}
		off += int64(h.size)
	}
	return nil
}

func readPayload(r io.ReaderAt, h boxHeader, limit int64) ([]byte, error) {
	n := int64(h.payloadLen())
	if n > limit {
		return nil, fmt.Errorf("box %q payload %d bytes exceeds limit %d", h.typ, n, limit)
	}
	buf := make([]byte, n)
	if _, err := r.ReadAt(buf, h.payloadOff()); err != nil {
		return nil, err
	}
	return buf, nil
}

// unmarshalBox parses a leaf box payload with go-mp4.
func unmarshalBox(payload []byte, dst mp4.IBox) error {
	_, err := mp4.Unmarshal(bytes.NewReader(payload), uint64(len(payload)), dst, mp4.Context{})
	return err
}

// readInto reads and unmarshals one leaf box.
func readInto(r io.ReaderAt, bh boxHeader, dst mp4.IBox) error {
	p, err := readPayload(r, bh, maxMoovBytes)
	if err != nil {
		return err
	}
	return unmarshalBox(p, dst)
}
