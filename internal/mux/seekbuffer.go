package mux

import "io"

// seekBuffer is a deterministic in-memory io.WriteSeeker used with mp4.Writer.
type seekBuffer struct {
	b   []byte
	pos int64
}

func (s *seekBuffer) Write(p []byte) (int, error) {
	end := s.pos + int64(len(p))
	if end > int64(len(s.b)) {
		grown := make([]byte, end-int64(len(s.b)))
		s.b = append(s.b, grown...)
	}
	copy(s.b[s.pos:end], p)
	s.pos = end
	return len(p), nil
}

func (s *seekBuffer) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = s.pos + offset
	case io.SeekEnd:
		target = int64(len(s.b)) + offset
	default:
		return 0, io.ErrUnexpectedEOF
	}
	if target < 0 {
		return 0, io.ErrUnexpectedEOF
	}
	s.pos = target
	return target, nil
}

func (s *seekBuffer) Bytes() []byte { return s.b }
