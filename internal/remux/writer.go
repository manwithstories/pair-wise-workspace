package remux

import (
	"io"

	"github.com/abema/go-mp4"
)

// byteWriteSeeker is an in-memory io.WriteSeeker used with go-mp4.Writer.
type byteWriteSeeker struct {
	buf []byte
	pos int64
}

func (w *byteWriteSeeker) Write(p []byte) (int, error) {
	end := w.pos + int64(len(p))
	if end > int64(len(w.buf)) {
		grown := make([]byte, end)
		copy(grown, w.buf)
		w.buf = grown
	}
	copy(w.buf[w.pos:end], p)
	w.pos = end
	return len(p), nil
}

func (w *byteWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		w.pos = offset
	case io.SeekCurrent:
		w.pos += offset
	case io.SeekEnd:
		w.pos = int64(len(w.buf)) + offset
	}
	if w.pos < 0 {
		w.pos = 0
	}
	return w.pos, nil
}

func (w *byteWriteSeeker) Bytes() []byte { return w.buf }

type boxFunc func(*mp4.Writer) error

func container(w *mp4.Writer, boxType mp4.BoxType, fn boxFunc) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: boxType}); err != nil {
		return err
	}
	if fn != nil {
		if err := fn(w); err != nil {
			return err
		}
	}
	_, err := w.EndBox()
	return err
}

func leaf(w *mp4.Writer, b mp4.IImmutableBox) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: b.GetType()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, b, mp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

func boxType(s string) mp4.BoxType { return mp4.StrToBoxType(s) }
