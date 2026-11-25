package ebml

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// PeekByte returns the next byte without consuming it. A clean end of stream
// is reported as io.EOF (no bytes left); a partial vint is reported as
// ErrTruncated by PeekID.
func (c *Cursor) PeekByte() (byte, error) {
	b, err := c.br.Peek(1)
	if err != nil {
		if errors.Is(err, io.EOF) && len(b) == 0 {
			return 0, io.EOF
		}
		if len(b) == 0 {
			return 0, fmt.Errorf("%w at offset %d", ErrTruncatedVint, c.off)
		}
		return b[0], nil
	}
	return b[0], nil
}

// PeekID peeks an element ID without consuming it. It returns io.EOF at a
// clean boundary and ErrTruncated when a multi-byte ID is cut short.
func (c *Cursor) PeekID() (Vint, error) {
	first, err := c.PeekByte()
	if err != nil {
		return Vint{Offset: c.off}, err
	}
	width, ok := vintWidth(first)
	if !ok || width > 4 {
		return Vint{Offset: c.off}, fmt.Errorf("%w at offset %d", ErrBadVint, c.off)
	}
	buf, err := c.br.Peek(width)
	if err != nil {
		if len(buf) < width {
			return Vint{Offset: c.off}, fmt.Errorf("%w: element id at offset %d truncated after %d/%d bytes",
				ErrTruncatedVint, c.off, len(buf), width)
		}
	}
	var val uint64
	for i := 0; i < width; i++ {
		val = val<<8 | uint64(buf[i])
	}
	return Vint{Value: val, Width: width, Offset: c.off}, nil
}

// PeekReader exposes the underlying bufio.Reader for callers that need
// peeking semantics (tests and diagnostics only).
func (c *Cursor) PeekReader() *bufio.Reader { return c.br }
