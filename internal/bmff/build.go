package bmff

import "encoding/binary"

// Builder is a small deterministic BMFF/MOV box encoder used by generated
// fixtures and the remux writer. It never encodes media samples itself; sample
// bytes are copied verbatim.
type Builder struct {
	Type     string
	Version  byte
	Flags    uint32
	Full     bool
	Payload  []byte
	Children []*Builder
	Force64  bool
}

func NewBox(typ string) *Builder { return &Builder{Type: typ} }

func NewFullBox(typ string, version byte, flags uint32, payload []byte) *Builder {
	return &Builder{Type: typ, Full: true, Version: version, Flags: flags, Payload: payload}
}

func (b *Builder) Add(children ...*Builder) *Builder {
	b.Children = append(b.Children, children...)
	return b
}

func (b *Builder) WithPayload(p []byte) *Builder {
	b.Payload = p
	return b
}

func (b *Builder) body() []byte {
	var body []byte
	if b.Full {
		body = make([]byte, 4+len(b.Payload))
		body[0] = b.Version
		body[1] = byte(b.Flags >> 16)
		body[2] = byte(b.Flags >> 8)
		body[3] = byte(b.Flags)
		copy(body[4:], b.Payload)
	} else {
		body = append([]byte(nil), b.Payload...)
	}
	for _, c := range b.Children {
		body = append(body, c.Build()...)
	}
	return body
}

func (b *Builder) Build() []byte {
	body := b.body()
	if b.Force64 || len(body)+16 > 0xffffffff {
		out := make([]byte, 16+len(body))
		binary.BigEndian.PutUint32(out[0:4], 1)
		putType(out[4:8], b.Type)
		binary.BigEndian.PutUint64(out[8:16], uint64(16+len(body)))
		copy(out[16:], body)
		return out
	}
	out := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(body)))
	putType(out[4:8], b.Type)
	copy(out[8:], body)
	return out
}

func putType(dst []byte, typ string) {
	for i := 0; i < 4; i++ {
		if i < len(typ) {
			dst[i] = typ[i]
		} else {
			dst[i] = ' '
		}
	}
}

// EncodeSTTS builds one compact run for fixtures whose sample durations are
// uniform.
func EncodeSTTS(count uint32, delta uint32) []byte {
	p := make([]byte, 12)
	binary.BigEndian.PutUint32(p[0:4], 1)
	binary.BigEndian.PutUint32(p[4:8], count)
	binary.BigEndian.PutUint32(p[8:12], delta)
	return p
}

func EncodeSTSZUniform(count, size uint32) []byte {
	p := make([]byte, 8)
	binary.BigEndian.PutUint32(p[0:4], size)
	binary.BigEndian.PutUint32(p[4:8], count)
	return p
}

func EncodeSTSCOne() []byte {
	p := make([]byte, 16)
	binary.BigEndian.PutUint32(p[0:4], 1)  // entry_count
	binary.BigEndian.PutUint32(p[4:8], 1)  // first_chunk
	binary.BigEndian.PutUint32(p[8:12], 1) // samples_per_chunk
	binary.BigEndian.PutUint32(p[12:16], 1)
	return p
}
