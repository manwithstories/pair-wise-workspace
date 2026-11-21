package ebml

import (
	"io"

	"mkvarchive/internal/model"
)

// Element is one EBML element header plus its absolute source offset.
// Data is only populated for elements read with ReadAll; streaming callers
// use Reader.ReadData / Skip instead to keep memory bounded.
type Element struct {
	ID         uint32
	Size       int64 // UnknownSize (-1) when the element uses unknown length
	HeaderOff  int64 // absolute offset of the ID byte
	DataOff    int64 // absolute offset of the first data byte
	HeaderSize int   // bytes occupied by ID + size VINTs
	Data       []byte
}

// End returns the absolute offset immediately after the element data, or -1
// for unknown-length elements whose end is delimited by the parent.
func (e *Element) End() int64 {
	if e.Size == UnknownSize {
		return -1
	}
	return e.DataOff + e.Size
}

// Reader is a streaming, offset-accounting EBML reader. It never buffers an
// entire input: callers decide per element whether to consume or skip data.
type Reader struct {
	r      io.Reader
	offset int64
}

// NewReader wraps r. The stream is assumed to start at absolute byte zero;
// use NewReaderAtOffset when parsing inside an already-consumed stream.
func NewReader(r io.Reader) *Reader { return &Reader{r: r} }

// NewReaderAtOffset wraps r whose next byte is at absOffset in the source.
func NewReaderAtOffset(r io.Reader, absOffset int64) *Reader {
	return &Reader{r: r, offset: absOffset}
}

// Offset returns the absolute source offset of the next unread byte.
func (r *Reader) Offset() int64 { return r.offset }

func (r *Reader) read(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(r.r, buf); err != nil {
		return nil, err
	}
	r.offset += int64(n)
	return buf, nil
}

// Next reads one element header without touching its data. A truncated ID or
// size VINT yields an EBML_TRUNCATED_HEADER error anchored at the exact byte
// where the broken header begins.
func (r *Reader) Next() (*Element, error) {
	idOff := r.offset
	idV, err := ReadElementID(r.r, idOff)
	if err != nil {
		return nil, err
	}
	r.offset += int64(idV.Width)
	if idV.Width > 4 {
		return nil, model.NewError("EBML_BAD_ELEMENT_ID",
			"element identifier exceeds 32 bits", idOff)
	}
	sizeOff := r.offset
	sizeV, err := ReadVINT(r.r, sizeOff)
	if err != nil {
		return nil, err
	}
	r.offset += int64(sizeV.Width)
	size := int64(sizeV.Value)
	if sizeV.Value == UnknownSizeValue(sizeV.Width) {
		size = UnknownSize
	}
	return &Element{
		ID:         uint32(idV.Value),
		Size:       size,
		HeaderOff:  idOff,
		DataOff:    r.offset,
		HeaderSize: idV.Width + sizeV.Width,
	}, nil
}

// ReadData consumes the element payload. An element whose declared size runs
// past end of input produces EBML_OVERRUN at the header offset while the
// bytes that were available stay with the caller via the returned buffer.
func (r *Reader) ReadData(e *Element) ([]byte, error) {
	if e.Size == UnknownSize {
		return nil, model.NewError("EBML_UNKNOWN_LENGTH",
			"element has unknown length; parse it as a master stream", e.HeaderOff)
	}
	buf, err := r.read(int(e.Size))
	if err != nil {
		return buf, model.NewError("EBML_OVERRUN",
			"declared element size extends past end of input", e.HeaderOff)
	}
	e.Data = buf
	return buf, nil
}

// Skip discards the element payload, accounting offsets identically to
// ReadData. Truncated tails are reported with the header offset.
func (r *Reader) Skip(e *Element) error {
	if e.Size == UnknownSize {
		return model.NewError("EBML_UNKNOWN_LENGTH",
			"cannot skip unknown-length element without parent bound", e.HeaderOff)
	}
	if _, err := io.CopyN(io.Discard, r.r, e.Size); err != nil {
		return model.NewError("EBML_OVERRUN",
			"declared element size extends past end of input", e.HeaderOff)
	}
	r.offset += e.Size
	return nil
}

// ChildReader returns a reader scoped to the payload of master element e,
// together with a limit that the caller must Close after walking children.
// Unknown-length masters are bounded by parentEnd (absolute source offset);
// pass -1 when the parent itself runs to end of input.
func (r *Reader) ChildReader(e *Element, parentEnd int64) (*Reader, io.Closer, error) {
	var remaining int64
	if e.Size != UnknownSize {
		remaining = e.Size
	} else if parentEnd >= 0 {
		remaining = parentEnd - r.offset
		if remaining < 0 {
			return nil, nil, model.NewError("EBML_OVERRUN",
				"master starts after parent end", e.HeaderOff)
		}
	} else {
		// Unbounded unknown-length master: parse until the underlying EOF.
		child := NewReaderAtOffset(r.r, r.offset)
		return child, &unboundCloser{parent: r, child: child}, nil
	}
	lr := io.LimitReader(r.r, remaining).(*io.LimitedReader)
	return NewReaderAtOffset(lr, r.offset), &limitCloser{
		parent: r, lr: lr, start: r.offset, length: remaining,
	}, nil
}

type limitCloser struct {
	parent *Reader
	lr     *io.LimitedReader
	start  int64
	length int64
}

func (c *limitCloser) Close() error {
	if c.lr.N > 0 {
		if _, err := io.CopyN(io.Discard, c.lr, c.lr.N); err != nil {
			return model.NewError("EBML_OVERRUN",
				"drain of master payload failed", c.parent.offset)
		}
	}
	c.parent.offset = c.start + c.length
	return nil
}

type unboundCloser struct {
	parent *Reader
	child  *Reader
}

func (c *unboundCloser) Close() error {
	c.parent.offset = c.child.offset
	return nil
}
