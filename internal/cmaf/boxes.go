// Package cmaf parses fragmented MP4 (CMAF) byte streams produced by the
// relay pipeline. It validates box boundaries defensively: any malformed
// input yields a BoxError carrying the absolute offset of the first damaged
// byte instead of a best-effort guess. Damaged data is never repaired here.
package cmaf

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Well-known box type codes used inside fMP4 fragments.
const (
	TypeFtyp = "ftyp"
	TypeMoof = "moof"
	TypeMfhd = "mfhd"
	TypeTraf = "traf"
	TypeTfhd = "tfhd"
	TypeTfdt = "tfdt"
	TypeTrun = "trun"
	TypeMdat = "mdat"
)

const (
	boxHeaderSize  = 8
	largeBoxHeader = 16
	largesizeMarker = uint32(1)
)

// Box is one parsed ISO-BMFF box. Data holds the box payload (bytes after
// the header), scoped to the original buffer; Start/End are absolute
// offsets within that buffer.
type Box struct {
	Type       string
	Start      int64
	HeaderSize int
	Size       uint64
	Data       []byte
}

// End returns the absolute offset immediately after the box.
func (b Box) End() int64 { return b.Start + int64(b.Size) }

// BoxError reports a structural defect. Offset is the absolute position of
// the first damaged (or first missing) byte in the scanned buffer.
type BoxError struct {
	Offset  int64
	BoxType string
	Reason  string
}

func (e *BoxError) Error() string {
	return fmt.Sprintf("cmaf: box %q damaged at offset %d: %s", e.BoxType, e.Offset, e.Reason)
}

// DamageOffset extracts the first-damaged-byte offset from a BoxError.
func DamageOffset(err error) (int64, bool) {
	var be *BoxError
	if errors.As(err, &be) {
		return be.Offset, true
	}
	return 0, false
}

// ScanBoxes walks the top-level boxes of data. base is the absolute offset
// of data[0] inside the enclosing stream and is added to every reported
// offset; pass 0 when data is the whole stream.
func ScanBoxes(data []byte, base int64) ([]Box, error) {
	var boxes []Box
	pos := 0
	for pos < len(data) {
		b, next, err := parseBox(data, pos, base)
		if err != nil {
			return nil, err
		}
		boxes = append(boxes, b)
		pos = next
	}
	return boxes, nil
}

// ChildBoxes parses the nested boxes contained in a container box payload.
func ChildBoxes(parent Box, base int64) ([]Box, error) {
	return ScanBoxes(parent.Data, base+int64(parent.HeaderSize))
}

func parseBox(data []byte, pos int, base int64) (Box, int, error) {
	if len(data)-pos < boxHeaderSize {
		return Box{}, 0, &BoxError{Offset: base + int64(pos), BoxType: "????",
			Reason: "truncated box header"}
	}
	rawSize := binary.BigEndian.Uint32(data[pos : pos+4])
	typ := string(data[pos+4 : pos+8])
	header := boxHeaderSize
	size := uint64(rawSize)
	if rawSize == largesizeMarker {
		if len(data)-pos < largeBoxHeader {
			return Box{}, 0, &BoxError{Offset: base + int64(pos), BoxType: typ,
				Reason: "truncated largesize header"}
		}
		size = binary.BigEndian.Uint64(data[pos+8 : pos+16])
		header = largeBoxHeader
	}
	// size 1 and 0 are reserved / to-EOF forms that the relay pipeline never
	// emits; accepting them would hide truncation, so both are rejected.
	if size < uint64(header) {
		return Box{}, 0, &BoxError{Offset: base + int64(pos), BoxType: typ,
			Reason: fmt.Sprintf("out-of-range box length %d", size)}
	}
	end := uint64(pos) + size
	if end > uint64(len(data)) {
		// The box header itself is the first corrupted unit: its declared
		// length cannot be satisfied by the remaining bytes. This covers a
		// truncated mdat exactly the same way as any other out-of-range
		// box, so callers always know where the damage begins.
		return Box{}, 0, &BoxError{Offset: base + int64(pos), BoxType: typ,
			Reason: fmt.Sprintf("out-of-range box length %d (only %d bytes remain)", size, len(data)-pos)}
	}
	b := Box{
		Type:       typ,
		Start:      base + int64(pos),
		HeaderSize: header,
		Size:       size,
		Data:       data[pos+header : pos+int(size)],
	}
	return b, pos + int(size), nil
}

// Find returns the first box of the given type, or false.
func Find(boxes []Box, typ string) (Box, bool) {
	for _, b := range boxes {
		if b.Type == typ {
			return b, true
		}
	}
	return Box{}, false
}
