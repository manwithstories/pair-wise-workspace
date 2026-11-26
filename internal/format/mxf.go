package format

import (
	"encoding/binary"
	"errors"

	"medialab/internal/model"
)

// MXFKey is a simplified 16-byte SMPTE-UL-style KLV key. The fixture subset
// distinguishes items by the final byte while keeping a real UL prefix.
type MXFKey [16]byte

var mxfULPrefix = [15]byte{
	0x06, 0x0e, 0x2b, 0x34, 0x01, 0x02, 0x01, 0x01,
	0x0d, 0x01, 0x03, 0x01, 0x15, 0x01, 0x01,
}

// MXFKeyFor returns the fixture key for a local item tag.
func MXFKeyFor(tag byte) MXFKey {
	var k MXFKey
	copy(k[:], mxfULPrefix[:])
	k[15] = tag
	return k
}

// Implemented MXF item tags.
const (
	MXFTagTrackID        byte = 0x10
	MXFTagKind           byte = 0x11
	MXFTagTimescale      byte = 0x12
	MXFTagDuration       byte = 0x13
	MXFTagDimensions     byte = 0x14
	MXFTagRotation       byte = 0x15
	MXFTagCrop           byte = 0x16
	MXFTagTrackColor     byte = 0x17
	MXFTagContainerColor byte = 0x18
	MXFTagKeyframes      byte = 0x19
)

// KLV is one encoded MXF universal-label item.
type KLV struct {
	Key   MXFKey
	Value []byte
}

// EncodeBERLength encodes a definite ASN.1 BER length.
func EncodeBERLength(n int) []byte {
	if n < 0 {
		panic("format: negative BER length")
	}
	if n < 0x80 {
		return []byte{byte(n)}
	}
	var buf [8]byte
	used := 0
	for v := n; v > 0; v >>= 8 {
		used++
		buf[8-used] = byte(v & 0xff)
	}
	out := make([]byte, 1+used)
	out[0] = 0x80 | byte(used)
	copy(out[1:], buf[8-used:])
	return out
}

// Encode encodes key, BER length and value.
func (k KLV) Encode() []byte {
	l := EncodeBERLength(len(k.Value))
	out := make([]byte, 0, 16+len(l)+len(k.Value))
	out = append(out, k.Key[:]...)
	out = append(out, l...)
	out = append(out, k.Value...)
	return out
}

// BuildMXF concatenates KLV items.
func BuildMXF(items ...KLV) []byte {
	var out []byte
	for _, item := range items {
		out = append(out, item.Encode()...)
	}
	return out
}

// ParseMXF parses the implemented KLV metadata subset.
func ParseMXF(data []byte) (*Container, error) {
	if len(data) == 0 {
		return nil, model.New(model.StageParseMXF, "empty_input", 0, 0, errTruncated)
	}
	if len(data) > MaxContainerBytes {
		return nil, model.New(model.StageParseMXF, "input_too_large", 0, 0, errTooLarge)
	}
	c := &Container{Format: "mxf", Raw: data}
	var current *Track
	pos := 0
	for pos < len(data) {
		start := int64(pos)
		if len(data)-pos < 16 {
			return nil, model.New(model.StageParseMXF, "truncated_key", currentTrackID(current), start, errTruncated)
		}
		var key MXFKey
		copy(key[:], data[pos:pos+16])
		lenStart := pos + 16
		length, lenSize, err := readBERLength(data, lenStart)
		if err != nil {
			return nil, model.New(model.StageParseMXF, "bad_ber_length", currentTrackID(current), start, err)
		}
		valueStart := lenStart + lenSize
		valueEnd := valueStart + length
		if length < 0 || valueStart < 0 || valueEnd < valueStart || valueEnd > len(data) {
			return nil, model.New(model.StageParseMXF, "klv_length_out_of_bounds", currentTrackID(current), start, errBadBoxLength)
		}
		value := data[valueStart:valueEnd]
		current, err = parseMXFItem(key, value, start, int64(valueStart), c, current)
		if err != nil {
			return nil, err
		}
		pos = valueEnd
	}
	if len(c.Tracks) == 0 {
		return nil, model.New(model.StageParseMXF, "no_tracks", 0, 0, errUnsupported)
	}
	return c, nil
}

func currentTrackID(t *Track) uint32 {
	if t == nil {
		return 0
	}
	return t.ID
}

func readBERLength(data []byte, pos int) (int, int, error) {
	if pos >= len(data) {
		return 0, 0, errTruncated
	}
	first := data[pos]
	if first < 0x80 {
		return int(first), 1, nil
	}
	n := int(first & 0x7f)
	if n == 0 {
		return 0, 0, errors.New("indefinite BER length is not supported")
	}
	if n > 4 {
		return 0, 0, errBadBERLength
	}
	if pos+1+n > len(data) {
		return 0, 0, errTruncated
	}
	var length uint64
	for i := 0; i < n; i++ {
		length = length<<8 | uint64(data[pos+1+i])
	}
	if length > uint64(MaxContainerBytes) {
		return 0, 0, errBadBERLength
	}
	return int(length), 1 + n, nil
}

func mxfUint32(v []byte, off int64, track uint32) (uint32, error) {
	if len(v) != 4 {
		return 0, model.New(model.StageParseMXF, "bad_uint32_value", track, off, errTruncated)
	}
	return binary.BigEndian.Uint32(v), nil
}
