package engine

import (
	"bytes"
	"errors"
	"io"
)

// bytesReader 包装 bytes.NewReader，便于在引擎内统一替换。
func bytesReader(b []byte) io.ReadSeeker { return bytes.NewReader(b) }

// newReader 从字节切片构造只读定位器。
func newReader(b []byte) io.ReadSeeker { return bytes.NewReader(b) }

// memReadWriteSeeker 是实现 io.ReadWriteSeeker 的内存文件，
// 供 go-mp4 在内存中封装 fMP4 段并回读核验。
type memReadWriteSeeker struct {
	b   []byte
	pos int64
}

func (m *memReadWriteSeeker) Write(p []byte) (int, error) {
	end := int(m.pos) + len(p)
	if end > len(m.b) {
		if cap(m.b) >= end {
			m.b = m.b[:end]
		} else {
			grown := make([]byte, end)
			copy(grown, m.b)
			m.b = grown
		}
	}
	copy(m.b[m.pos:end], p)
	m.pos = int64(end)
	return len(p), nil
}

func (m *memReadWriteSeeker) Read(p []byte) (int, error) {
	if int(m.pos) >= len(m.b) {
		return 0, errors.New("EOF")
	}
	n := copy(p, m.b[m.pos:])
	m.pos += int64(n)
	return n, nil
}

func (m *memReadWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var p int64
	switch whence {
	case io.SeekStart:
		p = offset
	case io.SeekCurrent:
		p = m.pos + offset
	case io.SeekEnd:
		p = int64(len(m.b)) + offset
	}
	if p < 0 {
		return 0, errors.New("negative seek")
	}
	m.pos = p
	return p, nil
}

func (m *memReadWriteSeeker) bytes() []byte { return append([]byte(nil), m.b...) }
