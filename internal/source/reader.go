package source

import (
	"fmt"
	"io"
	"os"
)

// Reader reads frames from a YUV420p file; safe for concurrent ReadFrame.
type Reader struct {
	f         *os.File
	width     int
	height    int
	frameSize int
	frames    int
}

// OpenReader opens path and validates its size against geometry.
func OpenReader(path string, w, h int) (*Reader, error) {
	if w < 16 || h < 16 || w%2 != 0 || h%2 != 0 {
		return nil, fmt.Errorf("invalid geometry %dx%d", w, h)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	fs := FrameSize(w, h)
	if st.Size()%int64(fs) != 0 || st.Size() == 0 {
		f.Close()
		return nil, fmt.Errorf("file size %d not a multiple of frame size %d", st.Size(), fs)
	}
	return &Reader{f: f, width: w, height: h, frameSize: fs, frames: int(st.Size() / int64(fs))}, nil
}

// Frames returns the total frame count.
func (r *Reader) Frames() int { return r.frames }

// Geometry returns width and height.
func (r *Reader) Geometry() (int, int) { return r.width, r.height }

// FrameSize returns bytes per frame.
func (r *Reader) FrameSize() int { return r.frameSize }

// ReadFrame reads frame index into dst (must be FrameSize bytes).
func (r *Reader) ReadFrame(index int, dst []byte) error {
	if index < 0 || index >= r.frames {
		return fmt.Errorf("frame %d out of range [0,%d)", index, r.frames)
	}
	if len(dst) < r.frameSize {
		return fmt.Errorf("dst too small: %d < %d", len(dst), r.frameSize)
	}
	_, err := r.f.ReadAt(dst[:r.frameSize], int64(index)*int64(r.frameSize))
	if err != nil && err != io.EOF {
		return err
	}
	return nil
}

// Close closes the underlying file.
func (r *Reader) Close() error { return r.f.Close() }
