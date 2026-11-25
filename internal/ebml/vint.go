package ebml

import (
	"errors"
	"fmt"
	"io"
)

// ReadID reads an element ID (marker bits retained), at most 4 bytes wide.
func (c *Cursor) ReadID() (Vint, error) {
	start := c.off
	first, err := c.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Vint{Offset: start}, io.EOF
		}
		return Vint{}, err
	}
	width, ok := vintWidth(first)
	if !ok || width > 4 {
		return Vint{Offset: start}, fmt.Errorf("%w at offset %d", ErrBadVint, start)
	}
	val := uint64(first)
	for i := 1; i < width; i++ {
		b, err := c.ReadByte()
		if err != nil {
			return Vint{Offset: start}, &posError{err: fmt.Errorf("%w: element id truncated after %d/%d bytes",
				ErrTruncatedVint, i, width), off: start}
		}
		val = val<<8 | uint64(b)
	}
	return Vint{Value: val, Width: width, Offset: start}, nil
}

// ReadSize reads an element size vint (marker bit stripped). The payload bits
// of the width byte are masked; all ones means unknown length.
func (c *Cursor) ReadSize() (Vint, error) {
	start := c.off
	first, err := c.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Vint{Offset: start}, io.EOF
		}
		return Vint{Offset: start}, err
	}
	width, ok := vintWidth(first)
	if !ok {
		return Vint{Offset: start}, fmt.Errorf("%w at offset %d", ErrBadVint, start)
	}
	mask := byte(1)<<(8-uint(width)) - 1
	val := uint64(first & mask)
	allOnes := val == uint64(mask)
	for i := 1; i < width; i++ {
		b, err := c.ReadByte()
		if err != nil {
			return Vint{Offset: start}, &posError{err: fmt.Errorf("%w: size vint truncated after %d/%d bytes",
				ErrTruncatedVint, i, width), off: start}
		}
		val = val<<8 | uint64(b)
		allOnes = allOnes && b == 0xFF
	}
	return Vint{Value: val, Width: width, Offset: start, Unknown: allOnes}, nil
}

// vintWidth counts leading zero bits: 1..8 bytes, zero invalid.
func vintWidth(b byte) (int, bool) {
	if b == 0 {
		return 0, false
	}
	for i := 0; i < 8; i++ {
		if b&(0x80>>uint(i)) != 0 {
			return i + 1, true
		}
	}
	return 0, false
}

// ReadElement reads the next element header.
func (c *Cursor) ReadElement() (Element, error) {
	head := c.off
	id, err := c.ReadID()
	if err != nil {
		return Element{}, err
	}
	size, err := c.ReadSize()
	if err != nil {
		return Element{}, err
	}
	el := Element{
		ID:       id.Value,
		Size:     int64(size.Value),
		Unknown:  size.Unknown,
		HeadOff:  head,
		BodyOff:  c.off,
		HeadSize: c.off - head,
	}
	if !size.Unknown && el.Size > c.lim.MaxElementSize {
		return el, &posError{err: fmt.Errorf("%w: element 0x%X declares %d bytes, cap %d",
			ErrTooLarge, el.ID, el.Size, c.lim.MaxElementSize), off: head}
	}
	return el, nil
}

// ReadBody loads a bounded element body.
func (c *Cursor) ReadBody(el Element) ([]byte, error) {
	if el.Unknown {
		return nil, &posError{err: fmt.Errorf("%w: element 0x%X has unknown size", ErrTooLarge, el.ID), off: el.HeadOff}
	}
	return c.ReadN(el.Size)
}
