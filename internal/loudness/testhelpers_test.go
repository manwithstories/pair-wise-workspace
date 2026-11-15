package loudness

import "bytes"

type testSink struct {
	buf []byte
	off int64
}

func (m *testSink) Write(p []byte) (int, error) {
	end := m.off + int64(len(p))
	if int(end) > len(m.buf) {
		grown := make([]byte, end)
		copy(grown, m.buf)
		m.buf = grown
	}
	copy(m.buf[m.off:], p)
	m.off = end
	return len(p), nil
}
func (m *testSink) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case 0:
		m.off = off
	case 1:
		m.off += off
	case 2:
		m.off = int64(len(m.buf)) + off
	}
	return m.off, nil
}
func (m *testSink) Close() error { return nil }

func bytesReader(data []byte) *bytes.Reader { return bytes.NewReader(data) }
