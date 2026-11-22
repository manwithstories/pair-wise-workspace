// Package fixture generates deterministic synthetic MP4/MOV material and RTP
// timestamp sidecars from a seed. No external media files are referenced.
package fixture

import (
	"encoding/binary"
	"io"
)

// BoxWriter serializes nested ISO BMFF boxes into an io.WriteSeeker,
// back-filling each box size when it closes.
type BoxWriter struct {
	w     io.WriteSeeker
	stack []boxFrame
}

type boxFrame struct {
	typ    string
	offset int64
}

// NewBoxWriter wraps a writer.
func NewBoxWriter(w io.WriteSeeker) *BoxWriter { return &BoxWriter{w: w} }

// Start writes a box header with a placeholder size.
func (b *BoxWriter) Start(typ string) error {
	off, err := b.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr, 0) // size patched on End
	copy(hdr[4:], []byte(typ))
	if _, err := b.w.Write(hdr); err != nil {
		return err
	}
	b.stack = append(b.stack, boxFrame{typ: typ, offset: off})
	return nil
}

// FullBox writes a version+flags header after Start.
func (b *BoxWriter) FullBox(version uint8, flags uint32) error {
	buf := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	_, err := b.w.Write(buf)
	return err
}

// Write writes raw box payload bytes.
func (b *BoxWriter) Write(p []byte) (int, error) { return b.w.Write(p) }

// End closes the most recently started box and patches its size.
func (b *BoxWriter) End() error {
	if len(b.stack) == 0 {
		return io.ErrUnexpectedEOF
	}
	frame := b.stack[len(b.stack)-1]
	b.stack = b.stack[:len(b.stack)-1]
	end, err := b.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	size := uint32(end - frame.offset)
	if _, err := b.w.Seek(frame.offset, io.SeekStart); err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], size)
	if _, err := b.w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = b.w.Seek(end, io.SeekStart)
	return err
}

// U16/U32/U64 append big-endian integers to dst and return the new slice.
func U16(dst []byte, v uint16) []byte {
	return append(dst, byte(v>>8), byte(v))
}
func U32(dst []byte, v uint32) []byte {
	return append(dst, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
func U64(dst []byte, v uint64) []byte {
	return append(dst, byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32),
		byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
