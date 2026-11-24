package parse

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"

	mp4 "github.com/abema/go-mp4"

	"mediarun/internal/model"
)

// Brand constants for files this pipeline produces.
var (
	majorBrand = [4]byte{'i', 's', 'o', 'm'}
	minorBrand = 512
)

// box is an in-memory box awaiting serialisation. Building the tree first
// and writing it afterwards keeps sizes exact: a container's size is its
// header plus its children.
type box struct {
	typ      [4]byte
	payload  []byte
	children []*box
}

func leaf(typ string, payload []byte) *box {
	if len(typ) != 4 {
		panic("parse: box type must be four bytes: " + typ)
	}
	return &box{typ: [4]byte{typ[0], typ[1], typ[2], typ[3]}, payload: payload}
}

// payloadBox is a container whose payload holds a fixed prefix (a full-box
// header written by go-mp4) followed by child boxes.
func payloadBox(typ string, payload []byte) *box {
	if len(typ) != 4 {
		panic("parse: box type must be four bytes: " + typ)
	}
	return &box{typ: [4]byte{typ[0], typ[1], typ[2], typ[3]}, payload: payload}
}

func container(typ string, children ...*box) *box {
	return payloadBox(typ, nil).withChildren(children...)
}

func (b *box) withChildren(children ...*box) *box {
	b.children = children
	return b
}

// encode returns the complete box bytes: an 8-byte header when the size fits
// in 32 bits, otherwise a 16-byte header.
func (b *box) encode() []byte {
	var body bytes.Buffer
	body.Write(b.payload)
	for _, c := range b.children {
		body.Write(c.encode())
	}
	total := uint64(8 + body.Len())
	if total > math.MaxUint32 {
		out := make([]byte, 16, 16+body.Len())
		binary.BigEndian.PutUint32(out[0:4], 1)
		copy(out[4:8], b.typ[:])
		binary.BigEndian.PutUint64(out[8:16], total+8)
		return append(out, body.Bytes()...)
	}
	out := make([]byte, 8, 8+body.Len())
	binary.BigEndian.PutUint32(out[0:4], uint32(total))
	copy(out[4:8], b.typ[:])
	return append(out, body.Bytes()...)
}

// marshalBody serialises a box payload with go-mp4, without a header.
func marshalBody(dst mp4.IImmutableBox) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := mp4.Marshal(&buf, dst, mp4.Context{}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// layoutSize is the number of sample bytes written inside mdat.
func (m *Movie) layoutSize() uint64 {
	var total uint64
	for _, t := range m.Tracks {
		for _, s := range t.Samples {
			total += uint64(len(s.Data))
		}
	}
	return total
}

// layout assigns every sample its absolute file offset, one chunk per
// sample in track order.
func (m *Movie) layout(start int64) ([][]int64, error) {
	pos := start
	out := make([][]int64, len(m.Tracks))
	for i, t := range m.Tracks {
		out[i] = make([]int64, len(t.Samples))
		for j, s := range t.Samples {
			if pos > math.MaxUint32 {
				return nil, fmt.Errorf("%w: sample offsets exceed 32-bit stco range", model.ErrLimit)
			}
			out[i][j] = pos
			pos += int64(len(s.Data))
		}
	}
	return out, nil
}

func sizeZero(m *Movie) [][]int64 {
	out := make([][]int64, len(m.Tracks))
	for i, t := range m.Tracks {
		out[i] = make([]int64, len(t.Samples))
	}
	return out
}
