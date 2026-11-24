// Package ebml implements a streaming EBML (Matroska/WebM) reader.
//
// The reader never seeks: it walks elements in document order and tracks
// master-element boundaries on an explicit stack. Elements with unknown
// data size (all VINT length bits set) are bounded by their parent frame
// and close automatically when the parent boundary is reached.
package ebml

import (
	"errors"
	"fmt"
	"io"
)

// Header describes one EBML element positioned at Offset.
type Header struct {
	ID     uint32
	Size   int64 // -1 when the data size is unknown
	Offset int64 // absolute offset of the element ID
	End    int64 // payload end, -1 when unknown
}

// IsMaster reports whether the element should be treated as a master
// (container) element. Matroska master elements carry no payload of their
// own; everything inside belongs to child elements.
func IsMaster(id uint32) bool {
	switch id {
	case IDEBML, IDSegment, IDInfo, IDTracks, IDTrackEntry,
		IDAudio, IDColour, IDCluster, IDBlockGroup:
		return true
	}
	return false
}

type frame struct {
	end       int64 // -1 for an unknown-length frame
	parentEnd int64 // enclosing frame end, -1 at document root
}

// Reader is a forward-only EBML stream parser.
type Reader struct {
	r      io.Reader
	pos    int64
	stack  []frame
	cur    Header
	loaded bool
}

// NewReader wraps r. The top-level document is itself an unknown-length
// frame that ends at EOF.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: r, stack: []frame{{end: -1, parentEnd: -1}}}
}

// Offset returns the absolute byte offset about to be read.
func (rd *Reader) Offset() int64 { return rd.pos }

func (rd *Reader) frameEnd() int64 { return rd.stack[len(rd.stack)-1].end }

// effectiveEnd returns the boundary that reads at the current depth must
// not cross: the frame end for sized frames, otherwise the parent frame
// end (unknown-size frames are bounded by their parent).
func (rd *Reader) effectiveEnd() int64 {
	top := rd.stack[len(rd.stack)-1]
	if top.end >= 0 {
		return top.end
	}
	return top.parentEnd
}

// read fills p fully. Frame-boundary violations become BoundaryError;
// short reads become TruncatedError naming the first missing byte, except
// a zero-byte read at the document root which returns io.EOF so callers
// can distinguish a clean document end from mid-frame truncation.
func (rd *Reader) read(p []byte, id uint32, what string) error {
	if len(p) == 0 {
		return nil
	}
	off := rd.pos
	end := rd.effectiveEnd()
	if end >= 0 && off+int64(len(p)) > end {
		return &BoundaryError{ElementID: id, Offset: off,
			Want: int64(len(p)), FrameEnd: end}
	}
	n, err := io.ReadFull(rd.r, p)
	rd.pos += int64(n)
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) && n == 0 && end < 0 {
		return io.EOF
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &TruncatedError{MissingOffset: off + int64(n), ElementID: id, What: what}
	}
	return err
}

// readID reads an element ID VINT. Unlike size VINTs, element IDs keep
// their length-marker bits in the numeric value (e.g. Segment 0x18538067).
func (rd *Reader) readID() (uint32, int, error) {
	off := rd.pos
	var first [1]byte
	if err := rd.read(first[:], 0, "element id"); err != nil {
		return 0, 0, err
	}
	b := first[0]
	if b == 0 {
		return 0, 0, fmt.Errorf("ebml: illegal VINT lead byte 0x00 at offset %d", off)
	}
	width := 1
	for m := byte(0x80); m&b == 0; m >>= 1 {
		width++
	}
	rest := make([]byte, width-1)
	if err := rd.read(rest, 0, "element id"); err != nil {
		return 0, 0, err
	}
	v := uint64(b) // retain marker bits
	for _, c := range rest {
		v = (v << 8) | uint64(c)
	}
	if width > 4 || v > reservedIDMarker {
		return 0, width, fmt.Errorf("ebml: invalid element id at offset %d", off)
	}
	return uint32(v), width, nil
}

// readVInt reads one EBML variable-length integer. When reservedOK is true
// the all-ones data form is accepted and ok is false (meaning "unknown").
func (rd *Reader) readVInt(id uint32, what string, reservedOK bool) (v uint64, n int, ok bool, err error) {
	off := rd.pos
	var first [1]byte
	if err = rd.read(first[:], id, what); err != nil {
		return 0, 0, false, err
	}
	b := first[0]
	if b == 0 {
		return 0, 0, false, fmt.Errorf("ebml: illegal VINT lead byte 0x00 at offset %d", off)
	}
	width := 1
	for m := byte(0x80); m&b == 0; m >>= 1 {
		width++
	}
	rest := make([]byte, width-1)
	if err = rd.read(rest, id, what); err != nil {
		return 0, 0, false, err
	}
	v = uint64(b & (0xFF >> uint(width)))
	for _, c := range rest {
		v = (v << 8) | uint64(c)
	}
	marker := uint64(1)<<uint(7*width) - 1
	if v == marker {
		if !reservedOK {
			return 0, width, false, fmt.Errorf("ebml: reserved VINT value at offset %d", off)
		}
		return 0, width, false, nil
	}
	return v, width, true, nil
}

// Next advances to the next element at the current depth and returns its
// header. io.EOF is returned when the current frame (or the document) ends.
func (rd *Reader) Next() (Header, error) {
	for {
		end := rd.effectiveEnd()
		if end >= 0 && rd.pos >= end {
			return Header{}, io.EOF
		}
		off := rd.pos
		id, _, err := rd.readID()
		if err != nil {
			return Header{}, err // io.EOF (clean root end) or TruncatedError
		}
		size, _, sizeOK, err := rd.readVInt(id, "element size", true)
		if err != nil {
			return Header{}, err
		}
		h := Header{ID: id, Offset: off, Size: -1, End: -1}
		if !sizeOK {
			// Reserved all-ones VINT: unknown data size, bounded by parent.
			rd.cur = h
			rd.loaded = true
			return h, nil
		}
		if size > MaxElementSize {
			return Header{}, &LimitError{ElementID: id, Size: size, Offset: off}
		}
		h.Size = int64(size)
		h.End = rd.pos + int64(size)
		if end >= 0 && h.End > end {
			return Header{}, &BoundaryError{ElementID: id, Offset: off,
				Want: h.End, FrameEnd: end}
		}
		rd.cur = h
		rd.loaded = true
		return h, nil
	}
}

// Enter pushes the current master element's frame. An unknown-size element
// inherits the parent frame; a sized element gets its own boundary. A
// declared frame extending past the effective parent boundary is rejected
// here so truncation surfaces at the entry rather than mid-walk.
func (rd *Reader) Enter(h Header) error {
	if !IsMaster(h.ID) {
		return fmt.Errorf("ebml: element 0x%X is not a master element", h.ID)
	}
	parent := rd.effectiveEnd()
	if h.End >= 0 && parent >= 0 && h.End > parent {
		return &BoundaryError{ElementID: h.ID, Offset: h.Offset,
			Want: h.End, FrameEnd: parent}
	}
	rd.stack = append(rd.stack, frame{end: h.End, parentEnd: parent})
	rd.loaded = false
	return nil
}

// Leave closes the current master frame and skips any unread bytes up to
// a known boundary. Unknown-length frames simply pop (their content is
// already bounded by the parent frame during reads).
func (rd *Reader) Leave() error {
	if len(rd.stack) <= 1 {
		return errors.New("ebml: unmatched Leave at document root")
	}
	top := rd.stack[len(rd.stack)-1]
	rd.stack = rd.stack[:len(rd.stack)-1]
	if top.end >= 0 && rd.pos < top.end {
		discard := top.end - rd.pos
		if err := rd.read(make([]byte, discard), 0, "frame skip"); err != nil {
			return err
		}
	}
	rd.loaded = false
	return nil
}

// ReadAll returns the full payload of the current non-master element and
// enforces both the element size and the enclosing frame boundary.
func (rd *Reader) ReadAll(h Header) ([]byte, error) {
	if IsMaster(h.ID) {
		return nil, fmt.Errorf("ebml: ReadAll on master element 0x%X", h.ID)
	}
	if h.Size < 0 {
		end := rd.effectiveEnd()
		if end < 0 {
			return nil, fmt.Errorf("ebml: unknown-size leaf element 0x%X at EOF frame", h.ID)
		}
		h.Size = end - rd.pos
	}
	buf := make([]byte, h.Size)
	if err := rd.read(buf, h.ID, "payload"); err != nil {
		return nil, err
	}
	rd.loaded = false
	return buf, nil
}

// Skip consumes the remainder of the current element without returning it.
func (rd *Reader) Skip(h Header) error {
	if h.Size < 0 {
		return fmt.Errorf("ebml: cannot skip unknown-size leaf 0x%X", h.ID)
	}
	remaining := h.End - rd.pos
	if remaining < 0 {
		return fmt.Errorf("ebml: element 0x%X already overrun", h.ID)
	}
	if remaining == 0 {
		rd.loaded = false
		return nil
	}
	err := rd.read(make([]byte, remaining), h.ID, "skip")
	rd.loaded = false
	return err
}
