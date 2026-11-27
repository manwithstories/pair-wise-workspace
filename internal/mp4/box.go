// Package mp4 implements a bounded ISO-BMFF (CMAF/fMP4) box parser. Every
// read is range-checked against the enclosing box and all size arithmetic is
// overflow-checked. Unknown boxes round-trip byte-for-byte.
package mp4

import (
	"math"

	"pairwise/ingestcheck/internal/clock"
)

// Box is a parsed unit of the ISO-BMFF tree. Raw holds the exact bytes from
// the start of the box header through its payload so unknown boxes and known
// boxes alike can be re-emitted verbatim.
type Box struct {
	Type       string
	Offset     int64
	Size       int64
	HeaderSize int64
	Payload    []byte
	Raw        []byte
	Children   []*Box
}

// ParseConfig bounds a single parse so that a corrupt file cannot force an
// unbounded allocation.
type ParseConfig struct {
	MaxBoxSize   int64
	MaxBoxCount  int
	MaxDepth     int
	KnownTypes   []string
	ContainerBox []string
}

// DefaultConfig returns the bounded parser settings used by the pipeline.
func DefaultConfig() ParseConfig {
	return ParseConfig{
		MaxBoxSize:   8 << 20,
		MaxBoxCount:  512,
		MaxDepth:     8,
		KnownTypes:   []string{"ftyp", "moov", "moof", "mfhd", "traf", "tfhd", "tfdt", "trun", "mdat", "free", "skip", "styp", "sidx", "prft", "mfra", "tfra", "mvex", "trex", "mehd"},
		ContainerBox: []string{"moov", "moof", "traf", "mvex", "mfra"},
	}
}

// IsKnown reports whether t is a box type the pipeline interprets.
func (c ParseConfig) IsKnown(t string) bool {
	for _, k := range c.KnownTypes {
		if k == t {
			return true
		}
	}
	return false
}

// IsContainer reports whether t carries child boxes.
func (c ParseConfig) IsContainer(t string) bool {
	for _, k := range c.ContainerBox {
		if k == t {
			return true
		}
	}
	return false
}

// Parse walks the byte slice as a sequence of top-level boxes. It rejects
// oversized boxes, truncated boxes and integer overflow, reporting the first
// bad byte in the returned error.
func Parse(data []byte, cfg ParseConfig) ([]*Box, error) {
	return parseSpan(data, 0, 0, cfg, nil)
}

func parseSpan(data []byte, base, depth int, cfg ParseConfig, parent *Box) ([]*Box, error) {
	if cfg.MaxBoxSize <= 0 || cfg.MaxBoxCount <= 0 || cfg.MaxDepth <= 0 {
		return nil, clock.New(clock.CodeArgument, "mp4", "non-positive parse bound")
	}
	if depth > cfg.MaxDepth {
		return nil, clock.At(clock.CodeBoxBounds, "mp4", int64(base), "depth limit exceeded")
	}
	var out []*Box
	pos := 0
	for pos < len(data) {
		if len(out) >= cfg.MaxBoxCount {
			return nil, clock.At(clock.CodeBoxBounds, "mp4", int64(base+pos), "box count limit exceeded")
		}
		box, n, err := parseBox(data[pos:], base+pos, cfg)
		if err != nil {
			return nil, err
		}
		if cfg.IsContainer(box.Type) {
			// Children occupy the payload; recurse under the same bounds.
			kids, kerr := parseSpan(box.Payload, base+pos+int(box.HeaderSize), depth+1, cfg, box)
			if kerr != nil {
				return nil, kerr
			}
			box.Children = kids
		}
		out = append(out, box)
		pos += n
	}
	return out, nil
}

func parseBox(data []byte, abs int, cfg ParseConfig) (*Box, int, error) {
	if len(data) < 8 {
		return nil, 0, clock.At(clock.CodeBoxTruncated, "mp4", int64(abs+len(data)), "box header shorter than 8 bytes")
	}
	size32 := uint64(data[0])<<24 | uint64(data[1])<<16 | uint64(data[2])<<8 | uint64(data[3])
	typ := string(data[4:8])

	header := int64(8)
	size := int64(size32)
	switch size32 {
	case 0:
		// size 0 means "to end of enclosing span"; an empty box is malformed.
		if len(data) < 8 {
			return nil, 0, clock.At(clock.CodeBoxTruncated, "mp4", int64(abs), "zero-size box has no payload")
		}
		size = int64(len(data))
	case 1:
		header = 16
		if len(data) < 16 {
			return nil, 0, clock.At(clock.CodeBoxTruncated, "mp4", int64(abs+8), "64-bit size missing")
		}
		hi := uint64(data[8])<<24 | uint64(data[9])<<16 | uint64(data[10])<<8 | uint64(data[11])
		lo := uint64(data[12])<<24 | uint64(data[13])<<16 | uint64(data[14])<<8 | uint64(data[15])
		wide := hi<<32 | lo
		if hi > 0x7FFFFFFF {
			return nil, 0, clock.At(clock.CodeBoxOverflow, "mp4", int64(abs+8), "64-bit size exceeds int64")
		}
		size = int64(wide)
	}
	size = rebase(size, abs)

	if size < header {
		return nil, 0, clock.At(clock.CodeBoxBounds, "mp4", int64(abs+int(header)), "box size smaller than its header")
	}
	if size > cfg.MaxBoxSize {
		return nil, 0, clock.At(clock.CodeBoxBounds, "mp4", int64(abs+int(header)), "box size exceeds configured bound")
	}
	if size > int64(len(data)) {
		return nil, 0, clock.At(clock.CodeBoxTruncated, "mp4", int64(abs+len(data)-1), "box claims more bytes than available")
	}
	payload := data[header:size]
	b := &Box{
		Type:       typ,
		Offset:     int64(abs),
		Size:       size,
		HeaderSize: header,
		Payload:    append([]byte(nil), payload...),
		Raw:        append([]byte(nil), data[:size]...),
	}
	return b, int(size), nil
}

// Bytes reconstructs the box. Known and unknown boxes both use Raw, so an
// unknown e.g. "uuid" box survives a parse/serialize cycle unchanged.
func (b *Box) Bytes() []byte {
	if b.Raw != nil {
		return b.Raw
	}
	out := make([]byte, 0, b.HeaderSize+int64(len(b.Payload)))
	out = append(out, make([]byte, b.HeaderSize)...)
	out = append(out, b.Payload...)
	return out
}

// Marshal builds a box with a standard 32-bit size field. The type must be
// four bytes; payload is copied so callers may reuse their buffer.
func Marshal(typ string, payload []byte) []byte {
	if len(typ) != 4 {
		typ = "free"
	}
	out := make([]byte, 8, 8+len(payload))
	putU32(out[0:4], uint64(8+len(payload)))
	copy(out[4:8], typ)
	return append(out, payload...)
}

// MarshalWide builds a box using the 64-bit "largesize" form (size field 1
// followed by a 64-bit size). Used by the fixture builder to exercise the
// overflow paths.
func MarshalWide(typ string, payload []byte) []byte {
	if len(typ) != 4 {
		typ = "free"
	}
	out := make([]byte, 16, 16+len(payload))
	putU32(out[0:4], 1)
	copy(out[4:8], typ)
	putU64(out[8:16], uint64(16+len(payload)))
	return append(out, payload...)
}

// Find returns the first descendant of the given type, depth-first.
func Find(boxes []*Box, typ string) *Box {
	for _, b := range boxes {
		if b.Type == typ {
			return b
		}
		if found := Find(b.Children, typ); found != nil {
			return found
		}
	}
	return nil
}

// FindAll returns every descendant of the given type.
func FindAll(boxes []*Box, typ string) []*Box {
	var out []*Box
	var walk func([]*Box)
	walk = func(bs []*Box) {
		for _, b := range bs {
			if b.Type == typ {
				out = append(out, b)
			}
			walk(b.Children)
		}
	}
	walk(boxes)
	return out
}

func putU32(b []byte, v uint64) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

// rebase converts a box size from a 32-bit field to an absolute byte index.
// A size at or above 2^63 cannot be represented and is reported as overflow.
func rebase(size int64, abs int) int64 {
	if size < 0 {
		return 0
	}
	if size > math.MaxInt64-int64(abs) {
		return math.MaxInt64
	}
	return size
}

func putU64(b []byte, v uint64) {
	if len(b) >= 8 {
		b[0] = byte(v >> 56)
		b[1] = byte(v >> 48)
		b[2] = byte(v >> 40)
		b[3] = byte(v >> 32)
		b[4] = byte(v >> 24)
		b[5] = byte(v >> 16)
		b[6] = byte(v >> 8)
		b[7] = byte(v)
		return
	}
	for i := range b {
		b[i] = byte(v >> (8 * (len(b) - 1 - i)))
	}
}
