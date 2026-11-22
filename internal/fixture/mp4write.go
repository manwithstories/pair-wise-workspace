package fixture

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/abema/go-mp4"
)

// memWS is an in-memory io.WriteSeeker supporting overwrite of already-written
// regions, so moov can be emitted first and stco offsets patched afterwards.
type memWS struct {
	buf []byte
	pos int
}

func (m *memWS) Write(p []byte) (int, error) {
	end := m.pos + len(p)
	if end > len(m.buf) {
		nb := make([]byte, end)
		copy(nb, m.buf)
		m.buf = nb
	}
	copy(m.buf[m.pos:], p)
	m.pos = end
	return len(p), nil
}

func (m *memWS) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = int64(m.pos) + offset
	case io.SeekEnd:
		abs = int64(len(m.buf)) + offset
	default:
		return 0, fmt.Errorf("bad whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("negative seek")
	}
	m.pos = int(abs)
	return abs, nil
}

func writeStsd(w *mp4.Writer) error {
	if err := startBox(w, "stsd"); err != nil {
		return err
	}
	writeRaw(w, []byte{0, 0, 0, 0})
	writeRaw(w, u32(1))
	if err := startBox(w, "avc1"); err != nil {
		return err
	}
	entry := make([]byte, 78)
	binary.BigEndian.PutUint16(entry[6:], 1)
	binary.BigEndian.PutUint16(entry[24:], 1920)
	binary.BigEndian.PutUint16(entry[26:], 1080)
	writeRaw(w, entry)
	if err := endBox(w); err != nil {
		return err
	}
	return endBox(w)
}

func startBox(w *mp4.Writer, typ string) error {
	_, err := w.StartBox(&mp4.BoxInfo{Type: mp4.StrToBoxType(typ), Size: 8, HeaderSize: 8})
	return err
}

func endBox(w *mp4.Writer) error {
	_, err := w.EndBox()
	return err
}

func writeBox(w *mp4.Writer, box mp4.IBox) error {
	payload := &bytes.Buffer{}
	if _, err := mp4.Marshal(payload, box, mp4.Context{}); err != nil {
		return fmt.Errorf("marshal %s: %w", box.GetType(), err)
	}
	if err := startBox(w, box.GetType().String()); err != nil {
		return err
	}
	writeRaw(w, payload.Bytes())
	return endBox(w)
}

func writeRaw(w *mp4.Writer, b []byte) {
	if _, err := w.Write(b); err != nil {
		panic(fmt.Sprintf("fixture write: %v", err))
	}
}

func u32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}
