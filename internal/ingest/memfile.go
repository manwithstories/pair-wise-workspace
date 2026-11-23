package ingest

import (
	"errors"
	"io"
)

// memFile is an in-memory io.WriteSeeker supporting backward seeks.
type memFile struct {
	buf []byte
	pos int64
}

func newMemFile(capHint int) *memFile { return &memFile{buf: make([]byte, 0, capHint)} }

func (m *memFile) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.buf)) {
		grow := make([]byte, end-int64(len(m.buf)))
		m.buf = append(m.buf, grow...)
	}
	copy(m.buf[m.pos:end], p)
	m.pos = end
	return len(p), nil
}

func (m *memFile) Seek(off int64, whence int) (int64, error) {
	var np int64
	switch whence {
	case io.SeekStart:
		np = off
	case io.SeekCurrent:
		np = m.pos + off
	case io.SeekEnd:
		np = int64(len(m.buf)) + off
	}
	if np < 0 {
		return 0, errors.New("negative seek")
	}
	if np > int64(len(m.buf)) {
		grow := make([]byte, np-int64(len(m.buf)))
		m.buf = append(m.buf, grow...)
	}
	m.pos = np
	return np, nil
}

func (m *memFile) Bytes() []byte { return m.buf }
