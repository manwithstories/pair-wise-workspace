package sidecar

import (
	"bytes"
	"errors"
	"io"
)

// memFile 是支持随机读写的内存文件，供 mp4.Writer 回跳修补盒大小。
type memFile struct {
	data []byte
	pos  int64
}

func newMemFile() *memFile { return &memFile{} }

func (m *memFile) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.data)) {
		m.data = append(m.data, make([]byte, end-int64(len(m.data)))...)
	}
	copy(m.data[m.pos:end], p)
	m.pos = end
	return len(p), nil
}

func (m *memFile) Read(p []byte) (int, error) {
	if m.pos >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[m.pos:])
	m.pos += int64(n)
	return n, nil
}

func (m *memFile) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = m.pos + offset
	case io.SeekEnd:
		target = int64(len(m.data)) + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if target < 0 {
		return 0, errors.New("negative position")
	}
	m.pos = target
	return target, nil
}

func (m *memFile) bytes() []byte { return m.data }

// 兼容历史命名，避免外部引用断裂。
func newBufferSeeker(_ *bytes.Buffer) *memFile { return newMemFile() }
