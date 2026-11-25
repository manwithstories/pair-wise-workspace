package fixture

import (
	"bytes"
	"encoding/binary"
	"math"

	"avcheck/internal/ebml"
)

// writer is a minimal EBML encoder with back-patched sizes.
type writer struct {
	buf     bytes.Buffer
	patches []int // buffer offsets of 8-byte size placeholders
}

func newWriter() *writer { return &writer{} }

func (w *writer) raw(b []byte) { w.buf.Write(b) }

func (w *writer) id(id uint64) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(id))
	n := idWidth(id)
	w.raw(b[4-n:])
}

func idWidth(id uint64) int {
	switch {
	case id >= 1<<24:
		return 4
	case id >= 1<<16:
		return 3
	case id >= 1<<8:
		return 2
	}
	return 1
}

// openMaster writes an ID and an 8-byte unknown-ish size placeholder that
// closeMaster patches. Using the 8-byte width keeps patching simple.
func (w *writer) openMaster(id uint64) *writer {
	w.id(id)
	w.patches = append(w.patches, w.buf.Len())
	w.raw([]byte{0x01, 0, 0, 0, 0, 0, 0, 0})
	return w
}

func (w *writer) closeMaster(_ *writer) {
	if len(w.patches) == 0 {
		return
	}
	pos := w.patches[len(w.patches)-1]
	w.patches = w.patches[:len(w.patches)-1]
	size := uint64(w.buf.Len() - pos - 8)
	// 8-byte vint: marker bit in the first byte plus 56 size bits.
	b := w.buf.Bytes()
	b[pos] = 0x01
	putUint56(b[pos+1:pos+8], size)
}

func putUint56(dst []byte, v uint64) {
	for i := 0; i < 7; i++ {
		dst[6-i] = byte(v >> (8 * uint(i)))
	}
}

func (w *writer) master(id uint64, body func(*writer)) {
	w.openMaster(id)
	body(w)
	w.closeMaster(nil)
}

func (w *writer) uint(id uint64, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	n := 1
	for n < 8 && v>>(8*uint(n)) != 0 {
		n++
	}
	w.id(id)
	w.uintSize(uint64(n))
	w.raw(b[8-n:])
}

// uintSize writes a non-unknown size vint of minimal width. Width n encodes
// 7n-1 payload bits (all-ones is reserved for "unknown"), so a value must
// stay strictly below 2^(7n)-1 to fit.
func (w *writer) uintSize(v uint64) {
	n := 1
	for n < 8 && v >= (uint64(1)<<(7*uint(n)))-1 {
		n++
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	b[8-n] |= 0x80 >> uint(n-1)
	w.raw(b[8-n:])
}

func (w *writer) str(id uint64, s string) {
	w.id(id)
	w.uintSize(uint64(len(s)))
	w.raw([]byte(s))
}

func (w *writer) float(id uint64, f float64) {
	w.id(id)
	w.uintSize(8)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], math.Float64bits(f))
	w.raw(b[:])
}

// simpleBlock writes a SimpleBlock element with a 1-byte track vint.
func (w *writer) simpleBlock(track uint64, rel int16, key bool, payload []byte) {
	w.id(ebml.IDSimpleBlock)
	w.uintSize(uint64(1 + 2 + 1 + len(payload)))
	w.raw([]byte{0x80 | byte(track)})
	var tb [2]byte
	binary.BigEndian.PutUint16(tb[:], uint16(rel))
	w.raw(tb[:])
	if key {
		w.raw([]byte{0x80})
	} else {
		w.raw([]byte{0x00})
	}
	w.raw(payload)
}
