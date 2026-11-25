package pack

import "io"

// seekBuffer is an in-memory io.WriteSeeker used with go-mp4's Writer.
type seekBuffer struct {
	b   []byte
	pos int
}

func (s *seekBuffer) Write(p []byte) (int, error) {
	end := s.pos + len(p)
	if end > len(s.b) {
		grown := make([]byte, end)
		copy(grown, s.b)
		s.b = grown
	}
	copy(s.b[s.pos:end], p)
	s.pos = end
	return len(p), nil
}

func (s *seekBuffer) Seek(offset int64, whence int) (int64, error) {
	var next int
	switch whence {
	case io.SeekStart:
		next = int(offset)
	case io.SeekCurrent:
		next = s.pos + int(offset)
	case io.SeekEnd:
		next = len(s.b) + int(offset)
	default:
		return 0, io.ErrUnexpectedEOF
	}
	if next < 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if next > len(s.b) {
		grown := make([]byte, next)
		copy(grown, s.b)
		s.b = grown
	}
	s.pos = next
	return int64(next), nil
}

func (s *seekBuffer) Bytes() []byte { return s.b }
