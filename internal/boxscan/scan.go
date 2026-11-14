// Package boxscan performs a zero-copy structural scan of an ISO-BMFF file.
// It only reads box headers and records absolute offsets; sample payloads are
// never loaded into memory by this layer.
package boxscan

import (
	"encoding/binary"
	"errors"
	"io"
	"os"

	"mp4replay/internal/report"
)

// Box is one scanned node. Children is populated only for container boxes.
type Box struct {
	Type     string
	Start    int64 // absolute offset of the box header
	Header   int64 // header length (8, 16 for largesize, 24 for uuid)
	DataOff  int64 // absolute offset of the first payload byte
	Size     int64 // total box size including the header
	End      int64 // absolute offset immediately after the box
	Parent   *Box
	Children []*Box
	UUID     []byte // 16 bytes when Type == "uuid"
}

// IsContainer reports whether the box type may hold nested boxes.
func IsContainer(t string) bool {
	switch t {
	case "moov", "trak", "mdia", "minf", "stbl", "edts", "dinf", "mvex",
		"moof", "traf", "mfra", "udta", "meta", "sinf", "schi", "ipro":
		return true
	}
	return false
}

// File wraps an open, read-only MP4 together with its scan result.
type File struct {
	Roots  []*Box
	Size   int64
	handle *os.File
}

// ReaderAt returns the concrete ReaderAt (os.File implements it).
func (f *File) ReaderAt() *os.File { return f.handle }

// Close releases the file handle.
func (f *File) Close() error { return f.handle.Close() }

// Stats collects scan counters.
type Stats struct {
	UnknownSkipped int
}

// Open opens path read-only and scans the whole box tree.
func Open(path string) (*File, error) {
	h, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, report.NewError(report.ExitFileRead, "open", err.Error())
	}
	st, err := h.Stat()
	if err != nil {
		h.Close()
		return nil, report.NewError(report.ExitFileRead, "stat", err.Error())
	}
	f := &File{handle: h, Size: st.Size()}
	if err := f.scan(); err != nil {
		h.Close()
		return nil, err
	}
	return f, nil
}

func (f *File) scan() error {
	roots, err := scanRange(f.handle, 0, f.Size, nil)
	if err != nil {
		return err
	}
	f.Roots = roots
	for _, b := range roots {
		if b.Type == "moof" || findFirst(b, "moof") != nil {
			return report.NewError(report.ExitBoxInvalid, "scan",
				"fragmented MP4 (moof) is not supported")
		}
	}
	return nil
}

func findFirst(b *Box, t string) *Box {
	for _, c := range b.Children {
		if c.Type == t {
			return c
		}
		if r := findFirst(c, t); r != nil {
			return r
		}
	}
	return nil
}

func scanRange(r io.ReaderAt, base, end int64, parent *Box) ([]*Box, error) {
	var out []*Box
	pos := base
	header := make([]byte, 16)
	for pos < end {
		if end-pos < 8 {
			return nil, report.NewError(report.ExitBoxInvalid, "scan",
				"truncated box header")
		}
		if _, err := r.ReadAt(header[:8], pos); err != nil {
			return nil, report.NewError(report.ExitBoxInvalid, "scan",
				"truncated box: "+err.Error())
		}
		size32 := int64(binary.BigEndian.Uint32(header[0:4]))
		typ := string(header[4:8])
		hdrLen := int64(8)
		size := size32
		if size32 == 1 { // 64-bit largesize
			if _, err := r.ReadAt(header[8:16], pos+8); err != nil {
				return nil, report.NewError(report.ExitBoxInvalid, "scan",
					"truncated 64-bit box size")
			}
			size = int64(binary.BigEndian.Uint64(header[8:16]))
			hdrLen = 16
		}
		if size32 < 0 || (size32 > 1 && size32 < 8) {
			return nil, report.NewError(report.ExitBoxInvalid, "scan",
				"box size smaller than header")
		}
		box := &Box{Type: typ, Start: pos, Header: hdrLen}
		if typ == "uuid" {
			uuid := make([]byte, 16)
			if end-pos < hdrLen+16 {
				return nil, report.NewError(report.ExitBoxInvalid, "scan",
					"truncated uuid box")
			}
			if _, err := r.ReadAt(uuid, pos+hdrLen); err != nil {
				return nil, report.NewError(report.ExitBoxInvalid, "scan",
					"truncated uuid payload")
			}
			box.UUID = uuid
			hdrLen += 16
			box.Header = hdrLen
		}
		if size == 0 { // box extends to end of file
			size = end - pos
		}
		if size < hdrLen {
			return nil, report.NewError(report.ExitBoxInvalid, "scan",
				"box size smaller than its header")
		}
		box.Size = size
		box.DataOff = pos + hdrLen
		box.End = pos + size
		if box.End > end {
			return nil, report.NewError(report.ExitBoxInvalid, "scan",
				"box extends beyond container/file boundary")
		}
		box.Parent = parent
		if IsContainer(typ) {
			children, err := scanRange(r, box.DataOff, box.End, box)
			if err != nil {
				return nil, err
			}
			box.Children = children
		}
		out = append(out, box)
		pos = box.End
	}
	return out, nil
}

// FindFirst returns the first box of the given type in a depth-first search.
func FindFirst(roots []*Box, t string) *Box {
	for _, b := range roots {
		if b.Type == t {
			return b
		}
		if r := findFirst(b, t); r != nil {
			return r
		}
	}
	return nil
}

// Child returns the direct child of b with the given type, or nil.
func Child(b *Box, t string) *Box {
	if b == nil {
		return nil
	}
	for _, c := range b.Children {
		if c.Type == t {
			return c
		}
	}
	return nil
}

// Path resolves a chain of direct child box types.
func Path(b *Box, types ...string) *Box {
	cur := b
	for _, t := range types {
		cur = Child(cur, t)
		if cur == nil {
			return nil
		}
	}
	return cur
}

// ReadPayload loads a leaf box body (without its header) into memory.
// All lengths are bound-checked against the box before allocation.
func ReadPayload(r io.ReaderAt, b *Box) ([]byte, error) {
	n := b.End - b.DataOff
	if n < 0 || n > (1<<30) {
		return nil, report.NewError(report.ExitBoxInvalid, "read",
			"box payload length out of bounds")
	}
	buf := make([]byte, n)
	if _, err := r.ReadAt(buf, b.DataOff); err != nil && !errors.Is(err, io.EOF) {
		return nil, report.NewError(report.ExitFileRead, "read", err.Error())
	}
	return buf, nil
}

// CountUnknown walks the tree and counts leaf boxes whose type is not known.
func CountUnknown(roots []*Box, known map[string]bool) int {
	n := 0
	var walk func(b *Box)
	walk = func(b *Box) {
		if len(b.Children) == 0 && !known[b.Type] && b.Type != "uuid" {
			n++
		}
		for _, c := range b.Children {
			walk(c)
		}
	}
	for _, b := range roots {
		walk(b)
	}
	return n
}

// MustContainMoov returns an error if no moov box exists.
func MustContainMoov(roots []*Box) error {
	if FindFirst(roots, "moov") == nil {
		return report.NewError(report.ExitBoxInvalid, "scan", "moov box missing")
	}
	return nil
}
