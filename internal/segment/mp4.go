package segment

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Fragmented MP4 box parsing. Only the boxes needed to recover sample
// timing and sync flags are handled: moov/mvex/trak/mdia/mdhd, moof/traf/
// tfhd/tfdt/trun and stbl/stsd/stss. Unknown boxes are skipped by size.

var (
	ErrBadMP4       = errors.New("segment: malformed mp4")
	ErrTruncated    = errors.New("segment: truncated mp4")
	ErrBadTimescale = errors.New("segment: invalid mp4 timescale")
)

// MP4Info is the timing metadata read from an fMP4 segment.
type MP4Info struct {
	Timescale int64
	Tracks    []MP4Track
	Truncated bool
}

// MP4Track holds per-track default sample data from tfhd.
type MP4Track struct {
	ID           uint32
	DefaultDur   uint32
	DefaultSize  uint32
	DefaultFlags uint32
	Samples      int
}

// SampleDesc is one trun sample entry.
type SampleDesc struct {
	Duration uint32
	Size     uint32
	Flags    uint32
	Offset   int32
}

type box struct {
	typ   string
	body  []byte
	start int
}

// FaultTimescale wraps an invalid timescale with its detail.
type FaultTimescale struct{ Detail string }

func (e *FaultTimescale) Error() string { return ErrBadTimescale.Error() + ": " + e.Detail }
func (e *FaultTimescale) Unwrap() error { return ErrBadTimescale }

func readBoxes(data []byte) ([]box, error) {
	var out []box
	pos := 0
	for pos < len(data) {
		if len(data)-pos < 8 {
			return out, fmt.Errorf("%w at %d", ErrTruncated, pos)
		}
		size := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		typ := string(data[pos+4 : pos+8])
		hdr := 8
		if size == 1 {
			if len(data)-pos < 16 {
				return out, fmt.Errorf("%w at %d", ErrTruncated, pos)
			}
			size = int(binary.BigEndian.Uint64(data[pos+8 : pos+16]))
			hdr = 16
		} else if size == 0 {
			size = len(data) - pos
		}
		if size < hdr || pos+size > len(data) {
			return out, fmt.Errorf("%w: box %q size %d at %d", ErrTruncated, typ, size, pos)
		}
		out = append(out, box{typ: typ, body: data[pos+hdr : pos+size], start: pos})
		pos += size
	}
	return out, nil
}

func findBox(data []byte, typ string) (box, bool) {
	boxes, err := readBoxes(data)
	if err != nil {
		return box{}, false
	}
	for _, b := range boxes {
		if b.typ == typ {
			return b, true
		}
	}
	return box{}, false
}
