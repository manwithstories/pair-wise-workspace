package ebml

import (
	"io"
	"math"

	"mediaproc/internal/model"
)

const (
	// MaxVINTBytes is the EBML VINT width used by WebM/Matroska.
	MaxVINTBytes = 8
	// MaxIDBytes is the maximum element ID width accepted here.
	MaxIDBytes = 4
	// DefaultMaxScan bounds byte-wise resynchronization after corruption.
	DefaultMaxScan int64 = 1 << 20
)

// VINT is a decoded EBML variable-length integer.
type VINT struct {
	Raw     uint64
	Value   uint64
	Length  int
	Offset  int64
	Unknown bool
}

// Element is a decoded EBML element header.
type Element struct {
	ID         uint32
	Offset     int64
	HeaderSize int
	// Size is -1 for an unknown-length element.
	Size    int64
	Unknown bool
}

// PayloadOffset returns the first payload byte.
func (e Element) PayloadOffset() int64 { return e.Offset + int64(e.HeaderSize) }

// End returns the first byte after a known-size element.
func (e Element) End() int64 { return e.PayloadOffset() + e.Size }

// Reader is an offset-aware streaming EBML header reader. It never reads an
// entire media file into memory.
type Reader struct {
	r      io.ReadSeeker
	offset int64
}

// NewReader creates a Reader at the current seek position of r.
func NewReader(r io.ReadSeeker) (*Reader, error) {
	pos, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	return &Reader{r: r, offset: pos}, nil
}

// Offset returns the current absolute byte offset.
func (r *Reader) Offset() int64 { return r.offset }

// Seek moves both the underlying seeker and the tracked offset.
func (r *Reader) Seek(offset int64) error {
	pos, err := r.r.Seek(offset, io.SeekStart)
	if err != nil {
		return err
	}
	r.offset = pos
	return nil
}

func (r *Reader) readFull(buf []byte, errorOffset int64) (int, error) {
	n, err := io.ReadFull(r.r, buf)
	r.offset += int64(n)
	if err == io.ErrUnexpectedEOF {
		return n, model.NewError(model.PhaseEBML, errorOffset, true, "truncated VINT/element", err)
	}
	if err != nil && err != io.EOF {
		return n, model.NewError(model.PhaseEBML, errorOffset, false, "read failed", err)
	}
	return n, err
}

// ReadVINT reads and decodes one EBML VINT. When asID is true the marker bit
// is retained because element IDs include it.
func (r *Reader) ReadVINT(asID bool) (VINT, error) {
	start := r.offset
	var first [1]byte
	if _, err := r.readFull(first[:], start); err != nil {
		return VINT{Offset: start}, err
	}
	if first[0] == 0x00 {
		return VINT{Offset: start, Length: 1}, model.NewError(
			model.PhaseEBML, start, true, "illegal VINT: all-zero length descriptor", nil)
	}

	length := 1
	for mask := byte(0x80); first[0]&mask == 0; mask >>= 1 {
		length++
		if length > MaxVINTBytes {
			return VINT{Offset: start}, model.NewError(
				model.PhaseEBML, start, true, "illegal VINT: length exceeds 8 bytes", nil)
		}
	}

	encoded := make([]byte, length)
	encoded[0] = first[0]
	if length > 1 {
		if _, err := r.readFull(encoded[1:], start); err != nil {
			return VINT{Offset: start, Length: length}, err
		}
	}

	var raw uint64
	for _, b := range encoded {
		raw = raw<<8 | uint64(b)
	}

	v := VINT{Raw: raw, Length: length, Offset: start}
	if asID {
		v.Value = raw
		return v, nil
	}

	marker := uint64(1) << uint(7*length)
	v.Value = raw &^ marker
	v.Unknown = v.Value == marker-1
	return v, nil
}

// ReadElementHeader decodes an element ID and its size VINT.
func (r *Reader) ReadElementHeader() (Element, error) {
	start := r.offset
	idv, err := r.ReadVINT(true)
	if err != nil {
		return Element{Offset: start}, err
	}
	if idv.Length > MaxIDBytes {
		return Element{Offset: start}, model.NewError(
			model.PhaseEBML, start, true, "element ID is wider than 4 bytes", nil)
	}

	sv, err := r.ReadVINT(false)
	if err != nil {
		return Element{Offset: start}, err
	}

	el := Element{
		ID:         uint32(idv.Value),
		Offset:     start,
		HeaderSize: int(r.offset - start),
		Unknown:    sv.Unknown,
	}
	switch {
	case sv.Unknown:
		el.Size = -1
	case sv.Value > math.MaxInt64:
		return el, model.NewError(model.PhaseEBML, start, false, "element size overflows int64", nil)
	default:
		el.Size = int64(sv.Value)
	}
	return el, nil
}

// ReadPayload reads a known-size payload while rejecting allocations above
// maxSize. Truncation errors retain the element header's original offset.
func (r *Reader) ReadPayload(el Element, maxSize int64) ([]byte, error) {
	if el.Unknown {
		return nil, model.NewError(model.PhaseEBML, el.Offset, false,
			"cannot buffer unknown-length element payload", nil)
	}
	if el.Size > maxSize {
		return nil, model.NewError(model.PhaseEBML, el.Offset, true,
			"element payload exceeds bounded read limit", nil)
	}
	if err := r.Seek(el.PayloadOffset()); err != nil {
		return nil, model.NewError(model.PhaseEBML, el.Offset, false, "seek to payload failed", err)
	}
	buf := make([]byte, el.Size)
	if _, err := r.readFull(buf, el.Offset); err != nil {
		return nil, err
	}
	return buf, nil
}

// SeekToEnd positions the reader immediately after a known-size element.
func (r *Reader) SeekToEnd(el Element) error {
	if el.Unknown {
		return model.NewError(model.PhaseEBML, el.Offset, false, "cannot seek end of unknown element", nil)
	}
	return r.Seek(el.End())
}

// FindNextElement performs a bounded resynchronization search for targetID.
// A candidate header is returned positioned at its payload; callers should
// additionally validate semantic children before treating it as a boundary.
func (r *Reader) FindNextElement(targetID uint32, maxScan int64) (Element, error) {
	if maxScan <= 0 {
		maxScan = DefaultMaxScan
	}
	searchStart := r.Offset()
	var lastErr error
	for scanned := int64(0); scanned < maxScan; scanned++ {
		candidate := searchStart + scanned
		if err := r.Seek(candidate); err != nil {
			return Element{}, model.NewError(model.PhaseEBML, candidate, true, "resync seek failed", err)
		}
		el, err := r.ReadElementHeader()
		if err == nil && el.ID == targetID && el.HeaderSize >= 4 {
			return el, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	return Element{}, model.NewError(model.PhaseEBML, searchStart, true,
		"no valid EBML resynchronization boundary within scan limit", lastErr)
}
