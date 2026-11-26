package output

import "bytes"

// seekableBuffer is an in-memory io.WriteSeeker. The mp4.Writer needs Seek
// to back-patch box sizes, which bytes.Buffer cannot provide.
type seekableBuffer struct {
	buf bytes.Buffer
	pos int64
}

func newSeekableBuffer() *seekableBuffer { return &seekableBuffer{} }

func (s *seekableBuffer) Write(p []byte) (int, error) {
	// Support back-patching at an earlier position.
	end := s.pos + int64(len(p))
	if end > int64(s.buf.Len()) {
		if s.pos > int64(s.buf.Len()) {
			return 0, errSeekGap
		}
		grow := make([]byte, end-int64(s.buf.Len()))
		if _, err := s.buf.Write(grow); err != nil {
			return 0, err
		}
	}
	raw := s.buf.Bytes()
	n := copy(raw[s.pos:], p)
	s.pos += int64(n)
	return n, nil
}

func (s *seekableBuffer) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case 0:
		target = offset
	case 1:
		target = s.pos + offset
	case 2:
		target = int64(s.buf.Len()) + offset
	}
	if target < 0 {
		return 0, errNegativeSeek
	}
	s.pos = target
	return target, nil
}

func (s *seekableBuffer) Bytes() []byte { return s.buf.Bytes() }

type seekError string

func (e seekError) Error() string { return string(e) }

const (
	errSeekGap      = seekError("seekableBuffer: cannot write past a gap")
	errNegativeSeek = seekError("seekableBuffer: negative position")
)
