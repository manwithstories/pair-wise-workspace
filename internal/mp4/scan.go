// Package mp4 performs bounded MP4/MOV box scanning and deterministic repairs.
package mp4

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"mediad/internal/media"
)

const (
	// MaxFileSize is the accepted local input ceiling, exactly 2 GiB.
	MaxFileSize int64 = 2 << 30
	// MaxTracks is the hard track limit.
	MaxTracks = 32
	// MaxBoxDepth prevents pathological container recursion.
	MaxBoxDepth = 64
)

// Box is a scanned ISO-BMFF box boundary.
type Box struct {
	Type       string `json:"type"`
	Offset     int64  `json:"offset"`
	Size       int64  `json:"size"`
	HeaderSize int64  `json:"header_size"`
	Children   []*Box `json:"children,omitempty"`
}

// End returns the exclusive byte offset of the box.
func (b *Box) End() int64 { return b.Offset + b.Size }

// IsContainer reports box types whose payload is itself a box tree.
func IsContainer(t string) bool {
	switch t {
	case "moov", "trak", "mdia", "minf", "stbl", "edts", "dinf", "udta", "mvex", "moof", "traf":
		return true
	default:
		return false
	}
}

// Find returns the first descendant path match.
func (b *Box) Find(path ...string) *Box {
	if len(path) == 0 {
		return b
	}
	for _, c := range b.Children {
		if c.Type == path[0] {
			return c.Find(path[1:]...)
		}
	}
	return nil
}

// All returns every box whose type matches.
func (b *Box) All(typ string) []*Box {
	var out []*Box
	var walk func(*Box)
	walk = func(x *Box) {
		if x.Type == typ {
			out = append(out, x)
		}
		for _, c := range x.Children {
			walk(c)
		}
	}
	walk(b)
	return out
}

// Scan strictly scans top-level and known container boundaries. An invalid
// length produces a structured fault whose ByteRange starts at the box header,
// the first byte implicated by the abnormal length.
func Scan(r io.ReadSeeker, fileSize int64) ([]*Box, error) {
	root, faults, err := scan(r, fileSize, false)
	if err != nil {
		return nil, err
	}
	if len(faults) > 0 {
		return nil, faults[0]
	}
	return root.Children, nil
}

// ScanSalvage clamps boxes that run past EOF and returns the boundary fault.
// It is intended for truncated-mdat recovery only; impossible header lengths
// remain fatal.
func ScanSalvage(r io.ReadSeeker, fileSize int64) ([]*Box, []*media.Fault, error) {
	root, faults, err := scan(r, fileSize, true)
	if err != nil {
		return nil, nil, err
	}
	return root.Children, faults, nil
}

func scan(r io.ReadSeeker, fileSize int64, salvage bool) (*Box, []*media.Fault, error) {
	if fileSize <= 0 {
		return nil, nil, media.NewFault(media.CodeInvalidInput, "empty media file").Fatal()
	}
	if fileSize > MaxFileSize {
		return nil, nil, media.NewFault(media.CodeFileTooLarge, "file size %d exceeds %d", fileSize, MaxFileSize).Fatal()
	}
	root := &Box{Type: "root", Offset: 0, Size: fileSize}
	var faults []*media.Fault
	if err := scanRange(r, 0, fileSize, 0, root, salvage, &faults); err != nil {
		return nil, nil, err
	}
	return root, faults, nil
}

func scanRange(r io.ReadSeeker, start, end int64, depth int, parent *Box, salvage bool, faults *[]*media.Fault) error {
	if depth > MaxBoxDepth {
		return media.NewFault(media.CodeBoxBounds, "box nesting exceeds depth %d", MaxBoxDepth).
			WithByteRange(start, 0).Fatal()
	}
	pos := start
	for pos < end {
		_, typ, headerSize, declaredEnd, err := readHeader(r, pos, end)
		if err != nil {
			return err
		}
		size := declaredEnd - pos
		fault := validateBoundary(typ, pos, headerSize, declaredEnd, end)
		if fault != nil {
			if !salvage || fault.Code != media.CodeTruncatedMDAT || typ != "mdat" {
				return fault
			}
			*faults = append(*faults, fault)
			size = end - pos
			declaredEnd = end
		}
		box := &Box{Type: typ, Offset: pos, Size: size, HeaderSize: headerSize}
		parent.Children = append(parent.Children, box)
		if IsContainer(typ) && box.Size > box.HeaderSize {
			payloadStart := pos + headerSize
			if err := scanRange(r, payloadStart, declaredEnd, depth+1, box, salvage, faults); err != nil {
				return err
			}
		}
		pos = declaredEnd
		// A zero-size salvage clamp can make pos==end; otherwise defend against
		// a scanner bug causing a non-advancing loop.
		if pos <= box.Offset && pos < end {
			return media.NewFault(media.CodeBoxBounds, "non-advancing box scan at %d", pos).
				WithBox(typ).WithByteRange(pos, headerSize).Fatal()
		}
	}
	return nil
}

func readHeader(r io.ReadSeeker, offset, limit int64) (header []byte, typ string, headerSize int64, declaredEnd int64, err error) {
	if offset < 0 || offset > limit || limit > MaxFileSize {
		return nil, "", 0, 0, media.NewFault(media.CodeBoxBounds, "invalid scan range offset=%d limit=%d", offset, limit).
			WithByteRange(offset, 0).Fatal()
	}
	available := limit - offset
	if available < 8 {
		return nil, "", 0, 0, media.NewFault(media.CodeBoxBounds, "truncated box header at byte %d (%d trailing bytes)", offset, available).
			WithByteRange(offset, available).Fatal()
	}
	hdr := make([]byte, 16)
	if _, err := r.Seek(offset, io.SeekStart); err != nil {
		return nil, "", 0, 0, fmt.Errorf("seek box header at %d: %w", offset, err)
	}
	n, err := io.ReadFull(r, hdr[:8])
	if err != nil {
		return nil, "", 0, 0, media.NewFault(media.CodeBoxBounds, "read box header at byte %d: %v", offset, err).
			WithByteRange(offset, int64(n)).Fatal()
	}
	size32 := binary.BigEndian.Uint32(hdr[:4])
	typ = string(hdr[4:8])
	headerSize = 8
	size64 := uint64(size32)
	if size32 == 1 {
		if available < 16 {
			return nil, "", 0, 0, media.NewFault(media.CodeBoxBounds, "missing 64-bit box size at byte %d", offset).
				WithBox(typ).WithByteRange(offset+8, available-8).Fatal()
		}
		if _, err := io.ReadFull(r, hdr[8:16]); err != nil {
			return nil, "", 0, 0, media.NewFault(media.CodeBoxBounds, "read 64-bit size at byte %d: %v", offset+8, err).
				WithBox(typ).WithByteRange(offset+8, 8).Fatal()
		}
		size64 = binary.BigEndian.Uint64(hdr[8:16])
		headerSize = 16
	} else if size32 == 0 {
		size64 = uint64(limit - offset)
	}
	if size64 < uint64(headerSize) {
		return nil, "", 0, 0, media.NewFault(media.CodeBoxBounds, "box %q size %d is smaller than header %d at byte %d", typ, size64, headerSize, offset).
			WithBox(typ).WithByteRange(offset, 4).WithOriginal(fmt.Sprintf("size=%d", size64)).Fatal()
	}
	if size64 > uint64(math.MaxInt64-offset) {
		return nil, "", 0, 0, media.NewFault(media.CodeBoxBounds, "box %q size %d overflows boundary arithmetic at byte %d", typ, size64, offset).
			WithBox(typ).WithByteRange(offset, 4).WithOriginal(fmt.Sprintf("size=%d", size64)).Fatal()
	}
	return hdr[:headerSize], typ, headerSize, offset + int64(size64), nil
}

func validateBoundary(typ string, offset, headerSize, end, fileEnd int64) *media.Fault {
	if end < offset+headerSize {
		return media.NewFault(media.CodeBoxBounds, "box %q ends before its header at byte %d", typ, offset).
			WithBox(typ).WithByteRange(offset, headerSize).Fatal()
	}
	if end > fileEnd {
		code := media.CodeBoxBounds
		msg := fmt.Sprintf("box %q at byte %d ends at %d beyond file end %d", typ, offset, end, fileEnd)
		if typ == "mdat" {
			code = media.CodeTruncatedMDAT
			msg = fmt.Sprintf("mdat at byte %d declares end %d but file ends at %d", offset, end, fileEnd)
		}
		return media.NewFault(code, "%s", msg).
			WithBox(typ).
			WithByteRange(offset, min64(headerSize, max64(0, fileEnd-offset))).
			WithOriginal(fmt.Sprintf("declared_end=%d actual_end=%d", end, fileEnd))
	}
	return nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
