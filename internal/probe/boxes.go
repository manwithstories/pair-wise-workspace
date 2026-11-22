// Package probe inspects fMP4 initialization and media segments without
// modifying them. Every structural problem is reported at an exact byte
// offset, and no duration is invented when timing cannot be established.
package probe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"mediad/internal/fault"
)

// Box is one top-level or child box header with bounds information.
type Box struct {
	Type        string
	Offset      int64
	Size        int64
	HeaderSize  int64
	Complete    bool
	ExtendToEOF bool
}

// PayloadOffset is the first byte after the box header.
func (b Box) PayloadOffset() int64 { return b.Offset + b.HeaderSize }

// End is the first byte after the box, clamped to the observed size.
func (b Box) End() int64 { return b.Offset + b.Size }

// Box limits keep malformed inputs from allocating without bound.
const (
	maxBoxes      = 200_000
	maxScanOffset = int64(2 << 30) // 2 GiB per segment
)

// ErrTruncatedHeader marks a tail shorter than a box header.
var ErrTruncatedHeader = errors.New("probe: truncated box header")

// scanBoxes walks consecutive boxes in [start,end) and reports faults for
// headers that claim to extend past the buffer or are structurally invalid.
func scanBoxes(r io.ReadSeeker, start, end int64, limit int) ([]Box, []fault.Fault) {
	var boxes []Box
	var faults []fault.Fault
	off := start
	for off < end {
		if len(boxes) >= limit {
			faults = append(faults, fault.MustNew(fault.KindLimitExceeded,
				fault.Location{Offset: off}, fmt.Sprintf("more than %d boxes", limit), false, false))
			break
		}
		if end-off < 8 {
			faults = append(faults, fault.MustNew(fault.KindTruncatedTail,
				fault.Location{Offset: off, Size: end - off}, "fewer than 8 bytes for a box header", true, false))
			break
		}
		var hdr [8]byte
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			faults = append(faults, fault.MustNew(fault.KindBoxHeader,
				fault.Location{Offset: off}, err.Error(), false, false))
			break
		}
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			faults = append(faults, fault.MustNew(fault.KindTruncatedTail,
				fault.Location{Offset: off}, "short read on box header", true, false))
			break
		}
		size := int64(binary.BigEndian.Uint32(hdr[0:4]))
		boxType := string(hdr[4:8])
		headerSize := int64(8)
		extend := false
		if size == 1 {
			if end-off < 16 {
				faults = append(faults, fault.MustNew(fault.KindTruncatedTail,
					fault.Location{Offset: off}, "large-size box header truncated", true, false))
				break
			}
			var large [8]byte
			if _, err := io.ReadFull(r, large[:]); err != nil {
				faults = append(faults, fault.MustNew(fault.KindTruncatedTail,
					fault.Location{Offset: off}, "short read on large-size header", true, false))
				break
			}
			size = int64(binary.BigEndian.Uint64(large[:]))
			headerSize = 16
			if size < 16 {
				faults = append(faults, fault.MustNew(fault.KindBoxHeader,
					fault.Location{Offset: off, Size: size}, "invalid large box size", true, false))
				break
			}
		} else if size == 0 {
			size = end - off
			extend = true
		} else if size < 8 {
			faults = append(faults, fault.MustNew(fault.KindBoxHeader,
				fault.Location{Offset: off, Size: size}, "box size smaller than header", true, false))
			break
		}
		box := Box{Type: boxType, Offset: off, Size: size, HeaderSize: headerSize, ExtendToEOF: extend}
		if size > end-off {
			faults = append(faults, fault.MustNew(fault.KindBoxOverflow,
				fault.Location{Offset: off, Size: size},
				fmt.Sprintf("box %q claims %d bytes, only %d remain", boxType, size, end-off), true, false))
			box.Complete = false
			box.Size = end - off
			boxes = append(boxes, box)
			break
		}
		box.Complete = true
		boxes = append(boxes, box)
		if size == 0 {
			break
		}
		off += size
	}
	return boxes, faults
}

// boxAt scans a payload region and returns the first box of a type.
func boxAt(r io.ReadSeeker, start, end int64, want string) (Box, bool) {
	boxes, _ := scanBoxes(r, start, end, 4096)
	for _, b := range boxes {
		if b.Type == want {
			return b, true
		}
	}
	return Box{}, false
}
