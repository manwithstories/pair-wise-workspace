package mp4

import (
	"bytes"
	"io"

	"github.com/abema/go-mp4"

	"mediamix/internal/core"
)

// boxWriter is a minimal sequential box writer over a bytes.Buffer.
type boxWriter struct {
	buf   *bytes.Buffer
	marks []int
}

func (w *boxWriter) startBox(typ mp4.BoxType) error {
	w.marks = append(w.marks, w.buf.Len())
	w.buf.Write([]byte{0, 0, 0, 0})
	w.buf.Write(typ[:])
	return nil
}

func (w *boxWriter) endBox() error {
	if len(w.marks) == 0 {
		return &core.Error{Code: core.CodeInternal, Detail: "unbalanced box writer", Fatal: true}
	}
	start := w.marks[len(w.marks)-1]
	w.marks = w.marks[:len(w.marks)-1]
	size := uint32(w.buf.Len() - start)
	b := w.buf.Bytes()[start : start+4]
	b[0] = byte(size >> 24)
	b[1] = byte(size >> 16)
	b[2] = byte(size >> 8)
	b[3] = byte(size)
	return nil
}

// writeBox writes a complete box with its payload.
func (w *boxWriter) writeBox(box mp4.IImmutableBox) error {
	if err := w.startBox(box.GetType()); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w.buf, box, mp4.Context{}); err != nil {
		return &core.Error{Code: core.CodeInternal, Detail: "marshal box: " + err.Error(), Fatal: true}
	}
	return w.endBox()
}

// writeFullBox is writeBox for full boxes (version/flags payloads).
func (w *boxWriter) writeFullBox(box mp4.IBox) error {
	return w.writeBox(box)
}

// writeBoxTo renders a single box to a buffer; used for ftyp.
func writeBox(buf *bytes.Buffer, box mp4.IImmutableBox) error {
	w := &boxWriter{buf: buf}
	return w.writeBox(box)
}

// byteReader adapts a byte slice to the io.ReadSeeker go-mp4 expects.
type byteReader struct {
	data []byte
	pos  int64
}

func newByteReader(data []byte) *byteReader { return &byteReader{data: data} }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += int64(n)
	return n, nil
}

func (r *byteReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.pos = offset
	case io.SeekCurrent:
		r.pos += offset
	case io.SeekEnd:
		r.pos = int64(len(r.data)) + offset
	}
	if r.pos < 0 {
		return 0, &core.Error{Code: core.CodeInternal, Detail: "negative seek", Fatal: true}
	}
	return r.pos, nil
}
