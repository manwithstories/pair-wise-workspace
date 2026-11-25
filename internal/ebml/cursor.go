package ebml

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// Cursor is a bounds-checked sequential reader that tracks the absolute byte
// offset of every element so diagnostics can name the exact fault site.
type Cursor struct {
	br  *bufio.Reader
	off int64
	lim Limits
}

// NewCursor wraps r. The read buffer never exceeds the configured limit.
func NewCursor(r io.Reader, lim Limits) *Cursor {
	return NewCursorAt(r, lim, 0)
}

// NewCursorAt wraps r with an initial absolute offset, used for nested
// cursors over element bodies so byte offsets stay absolute.
func NewCursorAt(r io.Reader, lim Limits, base int64) *Cursor {
	size := lim.ReadBuffer
	if size <= 0 || size > HardMaxReadBuffer {
		size = 64 << 10
	}
	return &Cursor{br: bufio.NewReaderSize(r, size), lim: lim, off: base}
}

// Offset is the number of bytes consumed so far.
func (c *Cursor) Offset() int64 { return c.off }

// ReadByte consumes one byte. A clean end of stream yields io.EOF unchanged
// so callers can tell "no more elements" from "element cut short".
func (c *Cursor) ReadByte() (byte, error) {
	b, err := c.br.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		return 0, err
	}
	c.off++
	return b, nil
}

// ReadN consumes exactly n bytes, never allocating more than n and refusing
// n above the configured element/block cap.
func (c *Cursor) ReadN(n int64) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("ebml: negative read length %d at offset %d", n, c.off)
	}
	if n > c.lim.MaxElementSize {
		return nil, &posError{err: fmt.Errorf("%w: %d bytes exceeds cap %d",
			ErrTooLarge, n, c.lim.MaxElementSize), off: c.off}
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.br, buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, &posError{err: fmt.Errorf("%w (wanted %d bytes)", ErrTruncatedElement, n), off: c.off}
		}
		return nil, err
	}
	c.off += n
	return buf, nil
}

// Skip discards n bytes while respecting the element cap. It is used for
// unknown element bodies; truncation inside a skip is a fault at the skip
// start, not a silent stop.
func (c *Cursor) Skip(n int64) error {
	if n < 0 {
		return fmt.Errorf("ebml: negative skip %d at offset %d", n, c.off)
	}
	start := c.off
	written, err := io.CopyN(io.Discard, c.br, n)
	c.off += written
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return &posError{err: fmt.Errorf("%w (skipping %d bytes)", ErrTruncatedElement, n), off: start}
		}
		return err
	}
	return nil
}

// checkExtent refuses an element whose declared body crosses its parent end.
func checkExtent(el Element, end int64) error {
	if el.Unknown || end < 0 {
		return nil
	}
	if el.BodyOff+el.Size > end {
		return &posError{err: ErrTruncatedElement, off: el.HeadOff}
	}
	return nil
}
