// Package isobmff parses the subset of ISO base media file format boxes that
// CMAF/fMP4 fragments use. Every length is validated against its parent region
// before use; no offset arithmetic is performed without an overflow check.
package isobmff

import (
	"encoding/binary"
	"fmt"
	"math"

	"archive/internal/media"
)

// Box is one parsed ISO-BMFF box. Payload excludes the header bytes.
type Box struct {
	Type      string
	Offset    int64
	Size      int64
	HeaderLen int64
	Payload   []byte
}

// End returns the exclusive end offset of the box.
func (b Box) End() int64 { return b.Offset + b.Size }

// Child begins parsing the box's payload as a container region.
func (b Box) Child() *Region {
	return &Region{Data: b.Payload, Base: b.Offset + b.HeaderLen, Limit: int64(len(b.Payload))}
}

// Region is a bounded byte window. Offsets reported by the parser are absolute
// (Base adjusts any region that starts inside a larger buffer).
type Region struct {
	Data  []byte
	Base  int64 // absolute file offset of Data[0]
	Limit int64 // length of Data
}

// NewRegion builds a top-level region over the whole file image.
func NewRegion(data []byte) *Region {
	return &Region{Data: data, Base: 0, Limit: int64(len(data))}
}

// MaxDepth bounds container nesting so a hostile file cannot recurse forever.
const MaxDepth = 8

// Err structural helper. Uses media.ErrFirstBadByte so recovery can report the
// same first_bad_byte for parser faults and payload truncation alike.
func bad(off int64, format string, args ...any) error {
	return &media.ErrFirstBadByte{Offset: off, Reason: fmt.Sprintf(format, args...)}
}

// IsBadOffset reports whether err carries a first_bad_byte.
func IsBadOffset(err error) bool {
	_, ok := media.FirstBadByteOffset(err)
	return ok
}

// Walk calls fn for each sibling box in the region. It stops and returns the
// first structural error; boxes already validated are delivered before it.
func (r *Region) Walk(depth int, fn func(Box) error) error {
	if depth > MaxDepth {
		return bad(r.Base, "box nesting exceeds %d", MaxDepth)
	}
	var pos int64
	for pos < r.Limit {
		remaining := r.Limit - pos
		if remaining < 8 {
			// Trailing padding smaller than a header is tolerated only when
			// it is zero; otherwise it is a truncated box header.
			rest := r.Data[pos:]
			for _, c := range rest {
				if c != 0 {
					return bad(r.Base+pos, "truncated box header (%d bytes)", remaining)
				}
			}
			return nil
		}
		box, err := r.readBox(pos, depth)
		if err != nil {
			return err
		}
		if fn != nil {
			if err := fn(box); err != nil {
				return err
			}
		}
		pos = box.Size + (box.Offset - r.Base)
	}
	return nil
}

// readBox validates one box header at region-relative offset pos.
func (r *Region) readBox(pos int64, depth int) (Box, error) {
	hdr := r.Data[pos : pos+8]
	size32 := int64(binary.BigEndian.Uint32(hdr[0:4]))
	typ := string(hdr[4:8])
	headerLen := int64(8)
	boxSize := size32
	abs := r.Base + pos

	switch {
	case size32 == 0:
		// size 0 extends to the end of the enclosing region.
		depthCheck := depth
		_ = depthCheck
		boxSize = r.Limit - pos
	case size32 == 1:
		if r.Limit-pos < 16 {
			return Box{}, bad(abs, "truncated largesize for %q", typ)
		}
		boxSize = int64(binary.BigEndian.Uint64(r.Data[pos+8 : pos+16]))
		headerLen = 16
		if boxSize < 16 {
			return Box{}, bad(abs, "largesize %d below header size for %q", boxSize, typ)
		}
	case size32 < 8:
		return Box{}, bad(abs, "box %q size %d invalid", typ, size32)
	}
	if boxSize < headerLen {
		return Box{}, bad(abs, "box %q size %d below header %d", typ, boxSize, headerLen)
	}
	// Overflow-safe bound check: pos < Limit already holds.
	if boxSize > r.Limit-pos {
		// The declared end lies past this region. Report the first byte that
		// could not be delivered as the error position.
		return Box{}, bad(r.Base+r.Limit, "box %q size %d overruns region (limit %d)", typ, boxSize, r.Base+r.Limit)
	}
	payload := r.Data[pos+headerLen : pos+boxSize]
	return Box{Type: typ, Offset: abs, Size: boxSize, HeaderLen: headerLen, Payload: payload}, nil
}

// ---- Typed payload readers -------------------------------------------------

// header parses the version/flags prologue plus a payload remainder.
func header(b Box) (version byte, flags uint32, rest []byte, err error) {
	if len(b.Payload) < 4 {
		return 0, 0, nil, bad(b.Offset, "%q payload %d too short for version/flags", b.Type, len(b.Payload))
	}
	version = b.Payload[0]
	flags = binary.BigEndian.Uint32(b.Payload[0:4]) & 0x00FFFFFF
	rest = b.Payload[4:]
	return version, flags, rest, nil
}

// need verifies the remainder holds at least n bytes.
func need(b Box, rest []byte, n int) error {
	if len(rest) < n {
		return bad(b.Offset+b.HeaderLen+int64(len(b.Payload)-len(rest)), "%q field truncated: need %d bytes, have %d", b.Type, n, len(rest))
	}
	return nil
}

// Mvhd is the subset of movie header fields the fan-in cares about.
type Mvhd struct {
	Timescale media.TimeScale
	Duration  uint64
}

// ParseMvhd decodes mvhd.
func ParseMvhd(b Box) (Mvhd, error) {
	v, _, rest, err := header(b)
	if err != nil {
		return Mvhd{}, err
	}
	if v == 1 {
		if err := need(b, rest, 4+8+8+4+8); err != nil {
			return Mvhd{}, err
		}
		return Mvhd{Timescale: media.TimeScale(binary.BigEndian.Uint32(rest[20:24])), Duration: binary.BigEndian.Uint64(rest[24:32])}, nil
	}
	if err := need(b, rest, 4+4+4+4+4); err != nil {
		return Mvhd{}, err
	}
	return Mvhd{Timescale: media.TimeScale(binary.BigEndian.Uint32(rest[12:16])), Duration: uint64(binary.BigEndian.Uint32(rest[16:20]))}, nil
}

// Tkhd is the subset of track header fields used for identity and sanity.
type Tkhd struct {
	TrackID uint32
}

// ParseTkhd decodes tkhd. The version determines where the track id sits.
func ParseTkhd(b Box) (Tkhd, error) {
	v, _, rest, err := header(b)
	if err != nil {
		return Tkhd{}, err
	}
	if v == 1 {
		if err := need(b, rest, 4+8+8+4+4+8); err != nil {
			return Tkhd{}, err
		}
		return Tkhd{TrackID: binary.BigEndian.Uint32(rest[20:24])}, nil
	}
	if err := need(b, rest, 4+4+4+4+4); err != nil {
		return Tkhd{}, err
	}
	return Tkhd{TrackID: binary.BigEndian.Uint32(rest[12:16])}, nil
}

// Mdhd is the subset of media header fields the parser needs.
type Mdhd struct {
	Timescale media.TimeScale
	Duration  uint64
	Language  uint16
}

// ParseMdhd decodes mdhd.
func ParseMdhd(b Box) (Mdhd, error) {
	v, _, rest, err := header(b)
	if err != nil {
		return Mdhd{}, err
	}
	if v == 1 {
		if err := need(b, rest, 4+8+8+4+4+4); err != nil {
			return Mdhd{}, err
		}
		return Mdhd{
			Timescale: media.TimeScale(binary.BigEndian.Uint32(rest[20:24])),
			Duration:  binary.BigEndian.Uint64(rest[24:32]),
			Language:  binary.BigEndian.Uint16(rest[36:38]),
		}, nil
	}
	if err := need(b, rest, 4+4+4+4+4); err != nil {
		return Mdhd{}, err
	}
	return Mdhd{
		Timescale: media.TimeScale(binary.BigEndian.Uint32(rest[12:16])),
		Duration:  uint64(binary.BigEndian.Uint32(rest[16:20])),
		Language:  binary.BigEndian.Uint16(rest[20:22]),
	}, nil
}

// Hdlr carries the handler type ("vide"/"soun") and a name.
type Hdlr struct {
	HandlerType string
	Name        string
}

// ParseHdlr decodes hdlr.
func ParseHdlr(b Box) (Hdlr, error) {
	v, _, rest, err := header(b)
	if err != nil {
		return Hdlr{}, err
	}
	if v != 0 {
		return Hdlr{}, bad(b.Offset, "hdlr version %d unsupported", v)
	}
	if err := need(b, rest, 4+4+12+1); err != nil {
		return Hdlr{}, err
	}
	return Hdlr{HandlerType: string(rest[4:8]), Name: string(rest[20:])}, nil
}

// Stsd describes the first sample entry: codec fourcc, config size, and
// visual dimensions when present.
type Stsd struct {
	Codec    string
	InitSize int
	Width    int
	Height   int
}

// ParseStsd decodes stsd just deep enough to find the first entry.
func ParseStsd(b Box) (Stsd, error) {
	v, _, rest, err := header(b)
	if err != nil {
		return Stsd{}, err
	}
	if v != 0 {
		return Stsd{}, bad(b.Offset, "stsd version %d unsupported", v)
	}
	entryCount := binary.BigEndian.Uint32(rest[0:4])
	if entryCount == 0 {
		return Stsd{Codec: ""}, nil
	}
	// Entry count is a count, not a byte length; validate it against the
	// region before slicing.
	entryRegion := Region{Data: rest[4:], Base: b.Offset + b.HeaderLen + 4, Limit: int64(len(rest) - 4)}
	var first Box
	err = entryRegion.Walk(0, func(box Box) error {
		if first.Type == "" {
			first = box
		}
		return nil
	})
	if err != nil {
		return Stsd{}, err
	}
	if first.Type == "" {
		return Stsd{}, bad(b.Offset, "stsd declares %d entries but none present", entryCount)
	}
	out := Stsd{Codec: first.Type, Width: 0, Height: 0}
	if first.Type == "avc1" || first.Type == "avc3" {
		// VisualSampleEntry: 6 reserved + 2 data_ref + 16 pre + 4 width/height
		if len(first.Payload) < 24 {
			return Stsd{}, bad(first.Offset, "%q sample entry truncated", first.Type)
		}
		out.Width = int(binary.BigEndian.Uint16(first.Payload[20:22]))
		out.Height = int(binary.BigEndian.Uint16(first.Payload[22:24]))
		if len(first.Payload) < 86 {
			return Stsd{}, bad(first.Offset, "%q sample entry truncated", first.Type)
		}
		out.InitSize = int(binary.BigEndian.Uint16(first.Payload[84:86]))
	}
	return out, nil
}

// Trex is a track default from mvex.
type Trex struct {
	TrackID      uint32
	SampleDur    uint32
	SampleSize   uint32
	SampleFlags  uint32
	SampleOffset int32
}

// ParseTrex decodes trex.
func ParseTrex(b Box) (Trex, error) {
	v, _, rest, err := header(b)
	if err != nil {
		return Trex{}, err
	}
	if v != 0 {
		return Trex{}, bad(b.Offset, "trex version %d unsupported", v)
	}
	if err := need(b, rest, 20); err != nil {
		return Trex{}, err
	}
	return Trex{
		TrackID:      binary.BigEndian.Uint32(rest[0:4]),
		SampleOffset: int32(binary.BigEndian.Uint32(rest[4:8])),
		SampleDur:    binary.BigEndian.Uint32(rest[8:12]),
		SampleSize:   binary.BigEndian.Uint32(rest[12:16]),
		SampleFlags:  binary.BigEndian.Uint32(rest[16:20]),
	}, nil
}

// safeAdd adds two non-negative offsets, refusing int64 overflow.
func safeAdd(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, &media.ErrFirstBadByte{Offset: a, Reason: "integer overflow in offset sum"}
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, &media.ErrFirstBadByte{Offset: a, Reason: "integer overflow in offset sum"}
	}
	return a + b, nil
}
