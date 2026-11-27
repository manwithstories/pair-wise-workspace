package fixture

import (
	"errors"
	"io"
)

// avSeekSize mirrors FFmpeg's AVSEEK_SIZE: the seek callback returns the
// total buffer size instead of moving the cursor.
const avSeekSize = 0x10000

// memSink is a seekable in-memory io.WriteSeeker backed by a growable byte
// slice. FFmpeg's MPEG-TS muxer rewrites headers after encoding, so plain
// append buffering is insufficient; writes must land at the current cursor
// and seeks must support SEEK_SET/CUR/END plus AVSEEK_SIZE.
type memSink struct {
	data []byte
	pos  int64
}

func (m *memSink) Write(p []byte) (int, error) {
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

func (m *memSink) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = offset
	case io.SeekCurrent:
		m.pos += offset
	case io.SeekEnd:
		m.pos = int64(len(m.data)) + offset
	case avSeekSize:
		return int64(len(m.data)), nil
	default:
		return 0, errors.New("memSink: unsupported whence")
	}
	if m.pos < 0 {
		return 0, errors.New("memSink: negative position")
	}
	return m.pos, nil
}

func (m *memSink) Bytes() []byte { return m.data }
