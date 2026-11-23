package ingest_test

import (
	"bytes"
	"io"
)

// memFile adapts an in-memory buffer to io.WriteSeeker for tests and for
// callers that build a container in memory.
type memFile struct {
	buf *bytes.Buffer
	off int64
}

func newMemFile(buf *bytes.Buffer) *memFile { return &memFile{buf: buf} }

func (m *memFile) Read(p []byte) (int, error) {
	if m.off >= int64(m.buf.Len()) {
		return 0, io.EOF
	}
	n := copy(p, m.buf.Bytes()[m.off:])
	m.off += int64(n)
	return n, nil
}

func (m *memFile) Write(p []byte) (int, error) {
	if m.off > int64(m.buf.Len()) {
		// Extend with zeros to the seek position.
		m.buf.Write(make([]byte, m.off-int64(m.buf.Len())))
	}
	end := m.off + int64(len(p))
	if end > int64(m.buf.Len()) {
		m.buf.Write(make([]byte, end-int64(m.buf.Len())))
	}
	copy(m.buf.Bytes()[m.off:end], p)
	m.off = end
	return len(p), nil
}

func (m *memFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.off = offset
	case io.SeekCurrent:
		m.off += offset
	case io.SeekEnd:
		m.off = int64(m.buf.Len()) + offset
	}
	if m.off < 0 {
		m.off = 0
	}
	return m.off, nil
}
