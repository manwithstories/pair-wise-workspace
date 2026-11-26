// Package samplegen creates small, legal-enough MP4/MOV fixtures and the exact
// mutations used by the deterministic demo. No external media files are used.
package samplegen

import "encoding/binary"

// Builder is a tiny deterministic ISO-BMFF box tree builder.
type Builder struct {
	Type     string
	Payload  []byte
	Children []*Builder
	Marker   string
}

// New creates a box with a raw payload.
func New(typ string, payload []byte, children ...*Builder) *Builder {
	return &Builder{Type: typ, Payload: payload, Children: children}
}

// Full creates a FullBox with version and 24 flags.
func Full(typ string, version byte, flags uint32, body []byte) *Builder {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return New(typ, append(head, body...))
}

// Mark records this box's absolute start offset in the serialization map.
func (b *Builder) Mark(name string) *Builder {
	b.Marker = name
	return b
}

// Built is serialized data plus named box offsets.
type Built struct {
	Data  []byte
	Marks map[string]int
}

// Build serializes the tree from offset zero.
func (b *Builder) Build() *Built {
	marks := map[string]int{}
	data := b.serialize(marks, 0)
	return &Built{Data: data, Marks: marks}
}

func (b *Builder) serialize(marks map[string]int, start int) []byte {
	if b.Marker != "" {
		marks[b.Marker] = start
	}
	payload := append([]byte(nil), b.Payload...)
	cursor := start + 8 + len(payload)
	for _, child := range b.Children {
		cd := child.serialize(marks, cursor)
		payload = append(payload, cd...)
		cursor += len(cd)
	}
	size := 8 + len(payload)
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(size))
	copy(out[4:8], []byte(b.Type))
	copy(out[8:], payload)
	return out
}

func u16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func appendBytes(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func patchU32(data []byte, offset int, v uint32) {
	binary.BigEndian.PutUint32(data[offset:offset+4], v)
}

func patchU64(data []byte, offset int, v uint64) {
	binary.BigEndian.PutUint64(data[offset:offset+8], v)
}
