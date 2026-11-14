// Package yuv implements bounded, streaming 8-bit YUV420p frame I/O.
package yuv

import (
	"errors"
	"fmt"
	"io"
)

var (
	ErrInvalidFrame = errors.New("invalid yuv frame")
	ErrShortFrame   = errors.New("short yuv frame")
)

// Frame owns tightly packed Y, U and V planes. Chroma planes are half width
// and half height. Frames are always validated before crossing package APIs.
type Frame struct {
	Width  int
	Height int
	Y      []byte
	U      []byte
	V      []byte
}

// NewFrame allocates a zero-valued frame.
func NewFrame(width, height int) (*Frame, error) {
	if width <= 0 || height <= 0 || width%2 != 0 || height%2 != 0 {
		return nil, fmt.Errorf("%w: dimensions %dx%d", ErrInvalidFrame, width, height)
	}
	return &Frame{
		Width:  width,
		Height: height,
		Y:      make([]byte, width*height),
		U:      make([]byte, (width/2)*(height/2)),
		V:      make([]byte, (width/2)*(height/2)),
	}, nil
}

// Validate verifies dimensions and exact plane sizes without reading pixels.
func (f *Frame) Validate() error {
	if f == nil {
		return fmt.Errorf("%w: nil frame", ErrInvalidFrame)
	}
	if f.Width <= 0 || f.Height <= 0 || f.Width%2 != 0 || f.Height%2 != 0 {
		return fmt.Errorf("%w: dimensions %dx%d", ErrInvalidFrame, f.Width, f.Height)
	}
	chroma := (f.Width / 2) * (f.Height / 2)
	if len(f.Y) != f.Width*f.Height || len(f.U) != chroma || len(f.V) != chroma {
		return fmt.Errorf("%w: plane sizes Y=%d U=%d V=%d", ErrInvalidFrame, len(f.Y), len(f.U), len(f.V))
	}
	return nil
}

// Size is the exact serialized byte size.
func (f *Frame) Size() int64 {
	if f == nil {
		return 0
	}
	return int64(len(f.Y) + len(f.U) + len(f.V))
}

// ReadFrame reads exactly one frame. A clean EOF is returned unwrapped only
// when no byte of the next frame was available.
func ReadFrame(r io.Reader, width, height int) (*Frame, error) {
	f, err := NewFrame(width, height)
	if err != nil {
		return nil, err
	}
	yLen := len(f.Y)
	buf := make([]byte, yLen+len(f.U)+len(f.V))
	n, err := io.ReadFull(r, buf)
	if err != nil {
		if errors.Is(err, io.EOF) && n == 0 {
			return nil, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: got %d of %d bytes", ErrShortFrame, n, len(buf))
		}
		return nil, err
	}
	copy(f.Y, buf[:yLen])
	copy(f.U, buf[yLen:yLen+len(f.U)])
	copy(f.V, buf[yLen+len(f.U):])
	return f, nil
}

// WriteFrame writes one complete frame in planar YUV420p order.
func WriteFrame(w io.Writer, f *Frame) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if err := writeAll(w, f.Y); err != nil {
		return err
	}
	if err := writeAll(w, f.U); err != nil {
		return err
	}
	return writeAll(w, f.V)
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

// Reader streams a bounded sequence of frames.
type Reader struct {
	r          io.Reader
	width      int
	height     int
	limit      int
	read       int
	frameBytes int64
}

// NewReader creates a frame reader. limit must be positive.
func NewReader(r io.Reader, width, height, limit int) (*Reader, error) {
	if r == nil || limit <= 0 {
		return nil, fmt.Errorf("%w: bad reader arguments", ErrInvalidFrame)
	}
	fb := int64(width) * int64(height) * 3 / 2
	if fb <= 0 || width%2 != 0 || height%2 != 0 {
		return nil, fmt.Errorf("%w: dimensions %dx%d", ErrInvalidFrame, width, height)
	}
	return &Reader{r: r, width: width, height: height, limit: limit, frameBytes: fb}, nil
}

// Next returns the next frame or io.EOF after the declared frame count.
func (r *Reader) Next() (*Frame, error) {
	if r.read >= r.limit {
		return nil, io.EOF
	}
	f, err := ReadFrame(r.r, r.width, r.height)
	if err != nil {
		return nil, err
	}
	r.read++
	return f, nil
}

// Index is the zero-based index that the next call to Next will read.
func (r *Reader) Index() int { return r.read }

// Remaining is the number of frames still allowed by the limit.
func (r *Reader) Remaining() int { return r.limit - r.read }

// FrameBytes is the exact expected size of one frame.
func (r *Reader) FrameBytes() int64 { return r.frameBytes }

// Writer streams frames and records the exact bytes emitted.
type Writer struct {
	w     io.Writer
	bytes int64
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

func (w *Writer) Write(f *Frame) error {
	if err := WriteFrame(w.w, f); err != nil {
		return err
	}
	w.bytes += f.Size()
	return nil
}

func (w *Writer) Bytes() int64 { return w.bytes }
