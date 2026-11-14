// Package yuv handles raw YUV420p frame storage and I/O.
package yuv

import (
	"fmt"
	"io"
	"os"
)

// Frame holds one YUV420p planar frame.
type Frame struct {
	W, H      int
	Y, Cb, Cr []byte
}

// NewFrame allocates a frame, rejecting non-positive or odd dimensions.
func NewFrame(w, h int) (*Frame, error) {
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("invalid dimensions %dx%d: width and height must be positive", w, h)
	}
	if w%2 != 0 || h%2 != 0 {
		return nil, fmt.Errorf("invalid dimensions %dx%d: YUV420p requires even width and height", w, h)
	}
	return &Frame{
		W:  w,
		H:  h,
		Y:  make([]byte, w*h),
		Cb: make([]byte, w*h/4),
		Cr: make([]byte, w*h/4),
	}, nil
}

// ChromaW returns the chroma plane width.
func (f *Frame) ChromaW() int { return f.W / 2 }

// ChromaH returns the chroma plane height.
func (f *Frame) ChromaH() int { return f.H / 2 }

// FrameSize returns the byte size of one YUV420p frame.
func FrameSize(w, h int) int { return w * h * 3 / 2 }

// Clone returns a deep copy of the frame.
func (f *Frame) Clone() *Frame {
	c := &Frame{W: f.W, H: f.H, Y: append([]byte(nil), f.Y...), Cb: append([]byte(nil), f.Cb...), Cr: append([]byte(nil), f.Cr...)}
	return c
}

// WriteTo serializes the frame as Y plane, then Cb, then Cr.
func (f *Frame) WriteTo(w io.Writer) (int64, error) {
	var n int64
	for _, p := range [][]byte{f.Y, f.Cb, f.Cr} {
		m, err := w.Write(p)
		n += int64(m)
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// ReadFrame reads exactly one frame; returns io.EOF if no bytes remain.
func ReadFrame(r io.Reader, w, h int) (*Frame, error) {
	f, err := NewFrame(w, h)
	if err != nil {
		return nil, err
	}
	for _, p := range [][]byte{f.Y, f.Cb, f.Cr} {
		if _, err := io.ReadFull(r, p); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// LoadSegment reads an entire raw YUV420p file into memory.
func LoadSegment(path string, w, h int) ([]*Frame, error) {
	if _, err := NewFrame(w, h); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("segment file %q: %w", path, err)
	}
	fs := int64(FrameSize(w, h))
	if info.Size() == 0 {
		return nil, fmt.Errorf("segment file %q: zero frames (empty file)", path)
	}
	if info.Size()%fs != 0 {
		return nil, fmt.Errorf("segment file %q: size %d is not a multiple of frame size %d", path, info.Size(), fs)
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	count := int(info.Size() / fs)
	frames := make([]*Frame, 0, count)
	for i := 0; i < count; i++ {
		f, err := ReadFrame(fh, w, h)
		if err != nil {
			return nil, fmt.Errorf("segment file %q frame %d: %w", path, i, err)
		}
		frames = append(frames, f)
	}
	return frames, nil
}
