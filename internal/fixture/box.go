package fixture

import (
	"encoding/binary"
	"io"
)

// BoxFn serializes one complete box (header + payload) into a writer.
type BoxFn func(w io.Writer) error

// Box starts a plain (non-full) box builder.
func Box(typ string, fn func(w io.Writer) error) BoxFn {
	var t [4]byte
	copy(t[:], typ)
	return func(w io.Writer) error { return writeOne(w, t, -1, 0, fn) }
}

// FullBox starts a version/flags box builder.
func FullBox(typ string, version byte, flags uint32, fn func(w io.Writer) error) BoxFn {
	var t [4]byte
	copy(t[:], typ)
	return func(w io.Writer) error { return writeOne(w, t, int(version), flags, fn) }
}

func u8(w io.Writer, v uint8)  { _, _ = w.Write([]byte{v}) }
func u16(w io.Writer, v uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	_, _ = w.Write(b[:])
}
func u32(w io.Writer, v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	_, _ = w.Write(b[:])
}
func u64(w io.Writer, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	_, _ = w.Write(b[:])
}
func raw(w io.Writer, b []byte) { _, _ = w.Write(b) }
func s4(w io.Writer, s string)  { _, _ = w.Write([]byte(s)) }

// WriteBoxes serializes box functions into w as a contiguous byte range.
func WriteBoxes(w io.Writer, boxes ...BoxFn) error {
	for _, b := range boxes {
		if err := b(w); err != nil {
			return err
		}
	}
	return nil
}

func writeOne(w io.Writer, typ [4]byte, version int, flags uint32, fn func(w io.Writer) error) error {
	var payload []byte
	buf := newBuffer()
	if version >= 0 {
		u32(buf, (uint32(version)<<24)|(flags&0x00FFFFFF))
	}
	if fn != nil {
		if err := fn(buf); err != nil {
			return err
		}
	}
	payload = buf.Bytes()
	u32(w, uint32(8+len(payload)))
	raw(w, typ[:])
	_, err := w.Write(payload)
	return err
}

type memBuf struct{ b []byte }

func newBuffer() *memBuf                    { return &memBuf{} }
func (m *memBuf) Write(p []byte) (int, error) { m.b = append(m.b, p...); return len(p), nil }
func (m *memBuf) Bytes() []byte             { return m.b }
