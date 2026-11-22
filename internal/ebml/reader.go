// Package ebml parses Matroska/WebM element structure without loading the
// file into memory.  It resolves unknown-length master elements (Segment and
// Cluster), reports truncation with source offsets and a recoverable
// boundary, and guarantees that no read ever crosses the end of the source.
package ebml

import (
	"fmt"
	"io"

	"archivecheck/internal/fault"
)

// Phase distinguishes the three visitor callbacks for one element.
type Phase int

const (
	// PhaseEnter is delivered for master elements before their children.
	PhaseEnter Phase = iota
	// PhaseLeaf is delivered once for non-master elements.
	PhaseLeaf
	// PhaseExit is delivered for master elements after their children.
	PhaseExit
)

// Element describes one EBML element's position in the source.
type Element struct {
	ID           ID
	Parent       ID
	Depth        int
	Level        int
	HeaderOffset int64
	HeaderSize   int64
	Size         SizeInfo
	// PayloadOffset is the first payload byte.
	PayloadOffset int64
	// PayloadEnd is -1 while an unknown-size master is being entered.
	PayloadEnd int64
	// Truncated reports that the declared payload was cut by the source end.
	Truncated bool
}

// PayloadLen returns the resolved payload length, or -1 when still unknown.
func (e Element) PayloadLen() int64 {
	if e.PayloadEnd < 0 {
		return -1
	}
	return e.PayloadEnd - e.PayloadOffset
}

// IsMaster reports whether the element contains child elements.
func (e Element) IsMaster() bool { return masterIDs[e.ID] }

// Visitor receives document-order events.  Returning an error stops the walk.
type Visitor func(phase Phase, el Element) error

// Options tune the reader.
type Options struct {
	// MaxSizeLength is the largest legal size field width.  Matroska defaults
	// to 8 and the EBML header can lower it; values above 8 are clamped.
	MaxSizeLength int
	// MaxDepth bounds master nesting; 0 selects 32.
	MaxDepth int
	// Faults receives structural damage.  A nil set disables reporting.
	Faults *fault.Set
}

// Reader is a bounds-checked EBML structure scanner over an io.ReaderAt.
type Reader struct {
	src      io.ReaderAt
	size     int64
	maxSize  int
	maxDepth int
	faults   *fault.Set
	// halt is the first offset where a structural error was found.  Once set,
	// every enclosing scan stops there so the damage is reported exactly once.
	halt int64
}

// NewReader returns a reader limited to size bytes of src.
func NewReader(src io.ReaderAt, size int64, opts Options) *Reader {
	if size < 0 {
		size = 0
	}
	maxSize := opts.MaxSizeLength
	if maxSize <= 0 || maxSize > 8 {
		maxSize = 8
	}
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 32
	}
	return &Reader{src: src, size: size, maxSize: maxSize, maxDepth: maxDepth, faults: opts.Faults, halt: -1}
}

// Size returns the bounded source size.
func (r *Reader) Size() int64 { return r.size }

// Faults returns the fault set (possibly nil).
func (r *Reader) Faults() *fault.Set { return r.faults }

// ReadAt reads at most n bytes at off, clamped to the source end.  Short
// results are not an error; callers compare len to the request.
func (r *Reader) ReadAt(off, n int64) ([]byte, error) {
	if off < 0 || n <= 0 || off >= r.size {
		return nil, nil
	}
	if off+n > r.size {
		n = r.size - off
	}
	buf := make([]byte, n)
	got, err := r.src.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return buf[:got], err
	}
	return buf[:got], nil
}

// headerAt parses one element header at pos.
func (r *Reader) headerAt(pos, limit int64) (el Element, err error) {
	remaining := limit - pos
	if remaining <= 0 {
		return Element{}, io.EOF
	}
	// An id can take up to 4 bytes and a size up to maxSize; read the full
	// potential header once, clamped to the bound.
	want := int64(4 + r.maxSize)
	if remaining < want {
		want = remaining
	}
	remaining = want
	buf, err := r.ReadAt(pos, remaining)
	if err != nil {
		return Element{}, err
	}
	id, idLen, err := ParseID(buf, 4)
	if err != nil {
		return Element{}, fmt.Errorf("at 0x%x: %w", pos, err)
	}
	if int64(len(buf)) < int64(idLen)+1 {
		return Element{}, fmt.Errorf("at 0x%x: truncated size field after %d-byte id", pos, idLen)
	}
	maxSize := r.maxSize
	if int64(len(buf))-int64(idLen) < int64(maxSize) {
		maxSize = len(buf) - idLen
	}
	sz, err := ParseSize(buf[idLen:], maxSize)
	if err != nil {
		return Element{}, fmt.Errorf("at 0x%x: %w", pos, err)
	}
	headerSize := int64(idLen + sz.Length)
	if pos+headerSize > limit {
		return Element{}, fmt.Errorf("at 0x%x: header of %d bytes crosses bound 0x%x", pos, headerSize, limit)
	}
	el = Element{
		ID:            id,
		HeaderOffset:  pos,
		HeaderSize:    headerSize,
		Size:          sz,
		PayloadOffset: pos + headerSize,
		PayloadEnd:    -1,
		Level:         levelOf(id),
	}
	if !sz.Unknown {
		end := el.PayloadOffset + int64(sz.Value)
		if sz.Value > uint64(limit-el.PayloadOffset) || end < el.PayloadOffset {
			el.PayloadEnd = limit
			el.Truncated = true
		} else {
			el.PayloadEnd = end
		}
	}
	return el, nil
}

// Walk scans the whole document and delivers events to v.
func (r *Reader) Walk(v Visitor) error {
	end, err := r.scan(0, r.size, -1, 0, 0, v, false)
	_ = end
	return err
}

// scan walks elements in [start,limit).  When unknownParent is true the
// container itself has unknown size, so a known-level sibling id terminates
// the scan without being consumed.  It returns the offset where scanning
// stopped.
func (r *Reader) scan(start, limit int64, parentLevel, depth int, parent ID, v Visitor, unknownParent bool) (int64, error) {
	pos := start
	if limit > r.size {
		limit = r.size
	}
	for pos < limit {
		if r.halt >= 0 && pos >= r.halt {
			return pos, nil
		}
		el, err := r.headerAt(pos, limit)
		if err != nil {
			if err == io.EOF {
				return pos, nil
			}
			r.reportTruncation(pos, limit, err)
			r.halt = pos
			return pos, nil
		}
		if unknownParent && el.Level >= 0 && el.Level <= parentLevel {
			return pos, nil
		}
		el.Parent = parent
		el.Depth = depth
		if el.Truncated && !el.IsMaster() {
			r.reportPayloadTruncation(el, limit)
			if v != nil {
				if err := v(PhaseLeaf, el); err != nil {
					return pos, err
				}
			}
			r.halt = el.PayloadEnd
			return el.PayloadEnd, nil
		}
		if el.Truncated {
			r.reportPayloadTruncation(el, limit)
		}
		if !el.IsMaster() {
			if v != nil {
				if err := v(PhaseLeaf, el); err != nil {
					return pos, err
				}
			}
			pos = el.PayloadEnd
			continue
		}
		if depth+1 > r.maxDepth {
			r.report(fault.KindBadElement, pos, "master nesting exceeds depth limit")
			return pos, nil
		}
		if v != nil {
			if err := v(PhaseEnter, el); err != nil {
				return pos, err
			}
		}
		var childEnd int64
		if el.Size.Unknown {
			if el.Level < 0 {
				r.report(fault.KindUnsizedElement, pos, "unknown size on element without a known nesting level")
				return pos, nil
			}
			childEnd, err = r.scan(el.PayloadOffset, limit, el.Level, depth+1, el.ID, v, true)
		} else {
			childEnd, err = r.scan(el.PayloadOffset, el.PayloadEnd, el.Level, depth+1, el.ID, v, false)
		}
		if err != nil {
			return pos, err
		}
		el.PayloadEnd = childEnd
		if v != nil {
			if err := v(PhaseExit, el); err != nil {
				return pos, err
			}
		}
		if childEnd <= pos {
			// No progress: malformed or empty master.
			if el.Size.Unknown {
				r.report(fault.KindBadElement, pos, "unknown-size master made no progress")
			}
			r.halt = pos
			return pos, nil
		}
		pos = childEnd
	}
	return pos, nil
}

// ReadElementAt parses a single element header at off without descending.
func (r *Reader) ReadElementAt(off int64) (Element, error) {
	return r.headerAt(off, r.size)
}

// ReadPayload copies up to max payload bytes of el, clamped to the source.
func (r *Reader) ReadPayload(el Element, max int64) ([]byte, error) {
	if el.PayloadOffset < 0 || el.PayloadOffset >= r.size {
		return nil, nil
	}
	n := el.PayloadLen()
	if n < 0 {
		return nil, fmt.Errorf("ebml: payload length unknown")
	}
	if n > max {
		n = max
	}
	return r.ReadAt(el.PayloadOffset, n)
}

func (r *Reader) report(kind fault.Kind, off int64, msg string) {
	if r.faults == nil {
		return
	}
	r.faults.Add(fault.New(fault.ModuleEBML, kind, fault.NewOffset(uint64(off)), msg))
}

func (r *Reader) reportTruncation(pos, limit int64, cause error) {
	if r.faults == nil {
		return
	}
	remaining := limit - pos
	msg := fmt.Sprintf("%v; element starts at 0x%x with %d byte(s) before end 0x%x; recoverable boundary 0x%x", cause, pos, remaining, limit, pos)
	r.faults.Add(fault.New(fault.ModuleEBML, fault.KindTruncated, fault.NewOffset(uint64(pos)), msg))
}

func (r *Reader) reportPayloadTruncation(el Element, limit int64) {
	if r.faults == nil {
		return
	}
	msg := fmt.Sprintf("element 0x%x declares payload to 0x%x beyond source end 0x%x; stopped at recoverable boundary 0x%x",
		el.ID, el.PayloadOffset+int64(el.Size.Value), limit, el.PayloadEnd)
	f := fault.New(fault.ModuleEBML, fault.KindTruncated, fault.NewOffset(uint64(el.HeaderOffset)), msg)
	// A truncated block's first bytes are usually readable, so the offending
	// track can be named even though the payload is cut.
	if el.ID == IDSimpleBlock || el.ID == IDBlock {
		if head, err := r.ReadAt(el.PayloadOffset, 8); err == nil && len(head) > 0 {
			if track, _, _, perr := ParseTrackNumber(head); perr == nil {
				f.OnTrack(track)
			}
		}
	}
	r.faults.Add(f)
}

// masterIDs lists Matroska master elements the scanner descends into.
var masterIDs = map[ID]bool{
	IDEBML: true, IDSegment: true, IDSeekHead: true, IDInfo: true,
	IDTracks: true, IDTrackEntry: true, IDVideo: true, IDAudio: true,
	IDCluster: true, IDBlockGroup: true, IDCues: true, IDTags: true,
	IDAttachments: true, IDChapters: true, IDSilentTracks: true,
}

// levelOf returns the Matroska nesting level for known ids, or -1.
func levelOf(id ID) int {
	switch id {
	case IDEBML, IDSegment:
		return 0
	case IDSeekHead, IDInfo, IDTracks, IDCluster, IDCues, IDTags, IDAttachments, IDChapters:
		return 1
	case IDTrackEntry, IDVideo, IDAudio, IDBlockGroup, IDTimestampScale,
		IDDuration, IDMuxingApp, IDWritingApp, IDCuePoint, IDVoid, IDCRC32,
		IDEncryptedBlock:
		return 2
	}
	// Header children and TrackEntry grandchildren are all deeper than the
	// masters that can appear unknown-size (Segment and Cluster).
	return -1
}
