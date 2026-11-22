package ebml

import (
	"io"

	"mediapack/internal/clock"
)

// Element describes one EBML element header as encountered in the
// stream. Bodies are never buffered here; Read/Discard consume them.
type Element struct {
	ID       uint64
	Size     int64    // -1 when Unknown is true
	Unknown  bool     // unknown-length (live) element
	HeaderOff int64   // absolute offset of the ID's first byte
	BodyOff  int64    // absolute offset of the first body byte
	EndOff   int64    // absolute offset just past the body (-1 unknown)
	HeaderLen int     // ID + size VINT byte count
	Parent   *Element // enclosing element, nil at top level
}

// scope tracks an open parent element while descending the tree.
type scope struct {
	el      *Element
	bodyEnd int64 // -1 for unknown-length parents
}

// Reader is a streaming EBML reader. It enforces a per-element size
// ceiling and keeps an absolute byte counter so every error can be
// anchored to a precise input offset.
type Reader struct {
	r          io.Reader
	off        int64
	maxElement int64 // maximum declared finite element size
	stack      []scope
	// current is the element most recently returned by Next while its
	// body has not been fully consumed or skipped.
	current *Element
	// pendingEOF makes the next Next call return io.EOF after an
	// unknown-length top-level scope reaches real EOF.
	atEOF bool
}

// NewReader creates a Reader with the given maximum finite element size
// (declared sizes above it produce clock.CatOversized errors).
func NewReader(r io.Reader, maxElementSize int64) *Reader {
	if maxElementSize <= 0 {
		maxElementSize = 1 << 30
	}
	return &Reader{r: r, maxElement: maxElementSize}
}

// Offset returns the number of bytes consumed from the input so far.
func (r *Reader) Offset() int64 { return r.off }

func (r *Reader) readByteN(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(r.r, buf); err != nil {
		// Anchor truncation at the byte where the read started.
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, clock.AtError(clock.CatTruncated, r.off,
				"needed %d bytes but stream ended", n)
		}
		return nil, err
	}
	r.off += int64(n)
	return buf, nil
}

// popClosed finishes any scopes whose finite body ends at or before the
// current offset. Returns the active parent scope (or nil at top level).
func (r *Reader) popClosed() *scope {
	for len(r.stack) > 0 {
		top := &r.stack[len(r.stack)-1]
		if top.bodyEnd >= 0 && r.off >= top.bodyEnd {
			r.stack = r.stack[:len(r.stack)-1]
			continue
		}
		break
	}
	if len(r.stack) == 0 {
		return nil
	}
	return &r.stack[len(r.stack)-1]
}

// Next advances to the next element header at the current depth,
// closing any parent scopes whose bodies have been fully consumed.
// io.EOF is returned at a clean end of input (or end of a known parent).
func (r *Reader) Next() (*Element, error) {
	if r.atEOF {
		return nil, io.EOF
	}
	parent := r.popClosed()
	if parent != nil && parent.bodyEnd >= 0 && r.off >= parent.bodyEnd {
		return nil, io.EOF
	}
	headerStart := r.off
	id, idLen, _, err := ReadVINT(r.r, true)
	if err != nil {
		// Real EOF at an empty boundary is clean; anything else is corrupt.
		if err == io.EOF {
			if parent == nil || parent.bodyEnd < 0 || r.off == parent.bodyEnd {
				r.atEOF = true
				return nil, io.EOF
			}
		}
		return nil, r.wrapReadErr(err, headerStart)
	}
	r.off += int64(idLen)
	sizeStart := r.off
	size, sizeLen, _, err := ReadVINT(r.r, false)
	if err != nil {
		return nil, r.wrapReadErr(err, sizeStart)
	}
	r.off += int64(sizeLen)

	el := &Element{
		ID:        id,
		HeaderOff: headerStart,
		BodyOff:   r.off,
		HeaderLen: idLen + sizeLen,
	}
	if parent != nil {
		el.Parent = parent.el
	}
	if IsUnknownSize(size, sizeLen) {
		el.Unknown = true
		el.Size = -1
		el.EndOff = -1
	} else {
		if size > uint64(r.maxElement) {
			return nil, clock.AtError(clock.CatOversized, headerStart,
				"declared size %d exceeds limit %d", size, r.maxElement)
		}
		if uint64(r.off) > ^uint64(0)-size {
			return nil, clock.AtError(clock.CatOverflow, headerStart,
				"end offset overflow for size %d", size)
		}
		el.Size = int64(size)
		el.EndOff = r.off + int64(size)
		// A finite child cannot extend past its finite parent.
		if parent != nil && parent.bodyEnd >= 0 && el.EndOff > parent.bodyEnd {
			return nil, clock.AtError(clock.CatBadPath, headerStart,
				"element ends at %d beyond parent end %d", el.EndOff, parent.bodyEnd)
		}
	}
	r.current = el
	return el, nil
}

func (r *Reader) wrapReadErr(err error, off int64) error {
	if me, ok := clock.AsMediaError(err); ok {
		if me.Offset < 0 {
			me.Offset = off
		}
		return me
	}
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return clock.AtError(clock.CatTruncated, off, "unexpected end inside element header")
	}
	return err
}

// Push marks el as an entered container; subsequent Next calls descend
// into it. Unknown-length containers stay open until Leave/EOF.
func (r *Reader) Push(el *Element) {
	r.stack = append(r.stack, scope{el: el, bodyEnd: el.EndOff})
}

// Leave closes the innermost scope. For finite elements the reader
// discards any unconsumed body bytes first. It is idempotent: a scope
// already auto-closed by Next (its body fully consumed) is a no-op.
func (r *Reader) Leave() error {
	if len(r.stack) == 0 {
		return nil
	}
	top := r.stack[len(r.stack)-1]
	r.stack = r.stack[:len(r.stack)-1]
	if top.bodyEnd >= 0 && r.off < top.bodyEnd {
		return r.discard(top.bodyEnd - r.off)
	}
	return nil
}

// Depth returns the current container nesting depth.
func (r *Reader) Depth() int { return len(r.stack) }

func (r *Reader) discard(n int64) error {
	// Discard in bounded chunks so a huge skip never allocates.
	var buf [4096]byte
	for n > 0 {
		chunk := n
		if chunk > int64(len(buf)) {
			chunk = int64(len(buf))
		}
		got, err := io.ReadFull(r.r, buf[:chunk])
		r.off += int64(got)
		n -= int64(got)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				// Anchor at the first byte the element declared but the
				// stream failed to provide.
				return clock.AtError(clock.CatTruncated, r.off+n,
					"element body truncated, %d bytes missing", n)
			}
			return err
		}
	}
	return nil
}

// DiscardBody skips the full body of an element returned by Next.
func (r *Reader) DiscardBody(el *Element) error {
	if el.Unknown {
		return clock.NewError(clock.CatBadPath, "cannot discard unknown-length element %x", el.ID)
	}
	remaining := el.EndOff - r.off
	if remaining < 0 {
		return clock.AtError(clock.CatBadPath, r.off, "reader already past element end")
	}
	return r.discard(remaining)
}

// MarkEOF notes that real EOF has been reached inside an unknown-length
// scope; the following Next returns io.EOF cleanly.
func (r *Reader) MarkEOF() { r.atEOF = true }
