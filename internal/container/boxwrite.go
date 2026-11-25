package container

import (
	"encoding/binary"
)

// boxWriter accumulates ISO BMFF boxes. Box sizes are back-patched so callers
// never have to compute them up front.
type boxWriter struct {
	buf []byte
}

func (w *boxWriter) bytes(v []byte) { w.buf = append(w.buf, v...) }

func (w *boxWriter) u8(v uint8) { w.buf = append(w.buf, v) }
func (w *boxWriter) u16(v uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

func (w *boxWriter) u32(v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

func (w *boxWriter) u64(v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

// pad appends n zero bytes.
func (w *boxWriter) pad(n int) { w.buf = append(w.buf, make([]byte, n)...) }

// box starts a box and returns a marker used by endBox.
func (w *boxWriter) box(typ string) int {
	start := len(w.buf)
	w.pad(8)
	copy(w.buf[start+4:start+8], typ)
	return start
}

// fullBox starts a FullBox with version and flags.
func (w *boxWriter) fullBox(typ string, version uint8, flags uint32) int {
	start := w.box(typ)
	w.u8(version)
	w.buf = append(w.buf, byte(flags>>16), byte(flags>>8), byte(flags))
	return start
}

// endBox back-patches the size of the box opened at start.
func (w *boxWriter) endBox(start int) {
	binary.BigEndian.PutUint32(w.buf[start:start+4], uint32(len(w.buf)-start))
}

// matrix writes a 3x3 fixed-point transform for a clockwise rotation.
func (w *boxWriter) matrix(deg int) {
	const one = 1 << 16
	var m [9]int32
	switch deg {
	case 90:
		m = [9]int32{0, one, 0, -one, 0, 0, 0, 0, 1 << 30}
	case 180:
		m = [9]int32{-one, 0, 0, 0, -one, 0, 0, 0, 1 << 30}
	case 270:
		m = [9]int32{0, -one, 0, one, 0, 0, 0, 0, 1 << 30}
	default:
		m = [9]int32{one, 0, 0, 0, one, 0, 0, 0, 1 << 30}
	}
	for _, v := range m {
		w.u32(uint32(v))
	}
}

// colr writes a colr box with nclx colour information.
func (w *boxWriter) colr(primaries, transfer, matrix uint16, fullRange bool) {
	start := w.box("colr")
	w.bytes([]byte("nclx"))
	w.u16(primaries)
	w.u16(transfer)
	w.u16(matrix)
	if fullRange {
		w.u8(1)
	} else {
		w.u8(0)
	}
	w.u8(0)
	w.endBox(start)
}

// clap writes a clean aperture expressed as integer ratios.
func (w *boxWriter) clap(width, height int) {
	start := w.box("clap")
	w.u32(uint32(width))
	w.u32(1)
	w.u32(uint32(height))
	w.u32(1)
	w.u32(0)
	w.u32(1)
	w.u32(0)
	w.u32(1)
	w.endBox(start)
}

// berLen encodes a BER length.
func berLenBytes(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	var tmp [8]byte
	i := 8
	for v := n; v > 0; v >>= 8 {
		i--
		tmp[i] = byte(v)
	}
	return append([]byte{0x80 | byte(8-i)}, tmp[i:]...)
}

// klvBytes assembles a KLV triplet.
func klvBytes(key, value []byte) []byte {
	out := make([]byte, 0, 16+len(value)+4)
	out = append(out, key...)
	out = append(out, berLenBytes(len(value))...)
	return append(out, value...)
}

// localItem encodes one tag-length-value item.
func localItem(tag uint16, value []byte) []byte {
	var head [2]byte
	binary.BigEndian.PutUint16(head[:], tag)
	out := append(head[:], berLenBytes(len(value))...)
	return append(out, value...)
}
