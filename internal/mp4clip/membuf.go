package mp4clip

import (
	"errors"
	"io"
)

// MemBuffer is a growable in-memory io.ReadWriteSeeker. go-mp4 patches box
// headers by seeking backwards, so an append-only buffer cannot back it;
// this buffer supports absolute-position overwrites while remaining purely
// in-memory and allocation-bounded by the written content size.
type MemBuffer struct {
	data []byte
	pos  int64
}

// NewMemBuffer creates an empty buffer.
func NewMemBuffer() *MemBuffer { return &MemBuffer{} }

// Write stores p at the current position, growing the buffer as needed.
func (m *MemBuffer) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.data)) {
		grown := make([]byte, end)
		copy(grown, m.data)
		m.data = grown
	}
	copy(m.data[m.pos:end], p)
	m.pos = end
	return len(p), nil
}

// Read copies data at the current position and advances it.
func (m *MemBuffer) Read(p []byte) (int, error) {
	if m.pos >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[m.pos:])
	m.pos += int64(n)
	return n, nil
}

// Seek implements io.Seeker over the in-memory slice.
func (m *MemBuffer) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0:
		m.pos = offset
	case 1:
		m.pos += offset
	case 2:
		m.pos = int64(len(m.data)) + offset
	}
	if m.pos < 0 {
		return 0, errors.New("negative seek position")
	}
	return m.pos, nil
}

// Bytes returns the buffered content.
func (m *MemBuffer) Bytes() []byte { return m.data }

// Len returns the content length.
func (m *MemBuffer) Len() int { return len(m.data) }
