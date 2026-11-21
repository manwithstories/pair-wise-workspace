package probe

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"

	"mediad/internal/model"
)

// Item is a decoded local-set property.
type Item struct {
	Tag  uint16
	Prop Prop
	Val  []byte
	Off  uint64 // absolute file offset of the value, for diagnostics
}

// StrongRef is a typed reference to a metadata set or track.
type StrongRef struct {
	Type    UL
	UMID    [32]byte
	TrackID uint32
}

// LocalSet is a decoded KLV structured-metadata set.
type LocalSet struct {
	Key     UL
	FileOff uint64
	Length  uint64
	Items   []Item
}

// Get returns the first item with the given property name.
func (ls *LocalSet) Get(name string) (Item, bool) {
	for _, it := range ls.Items {
		if it.Prop.Name == name {
			return it, true
		}
	}
	return Item{}, false
}

// UMID returns the set's package UMID, if it carries one.
func (ls *LocalSet) UMID() ([32]byte, bool) {
	it, ok := ls.Get("PackageUMID")
	if !ok || len(it.Val) != 32 {
		return [32]byte{}, false
	}
	var id [32]byte
	copy(id[:], it.Val)
	return id, true
}

// Uint32 decodes a uint32 property.
func (ls *LocalSet) Uint32(name string) (uint32, bool) {
	it, ok := ls.Get(name)
	if !ok || len(it.Val) != 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(it.Val), true
}

// Int32 decodes an int32 property.
func (ls *LocalSet) Int32(name string) (int32, bool) {
	v, ok := ls.Uint32(name)
	return int32(v), ok
}

// Uint64 decodes a uint64 property.
func (ls *LocalSet) Uint64(name string) (uint64, bool) {
	it, ok := ls.Get(name)
	if !ok || len(it.Val) != 8 {
		return 0, false
	}
	return binary.BigEndian.Uint64(it.Val), true
}

// Rational decodes a numerator/denominator pair.
func (ls *LocalSet) Rational(name string) (num, den int32, ok bool) {
	it, ok := ls.Get(name)
	if !ok || len(it.Val) != 8 {
		return 0, 0, false
	}
	return int32(binary.BigEndian.Uint32(it.Val)), int32(binary.BigEndian.Uint32(it.Val[4:])), true
}

// String decodes a UTF-16BE string property.
func (ls *LocalSet) String(name string) (string, bool) {
	it, ok := ls.Get(name)
	if !ok || len(it.Val)%2 != 0 {
		return "", false
	}
	units := make([]uint16, len(it.Val)/2)
	for i := range units {
		units[i] = binary.BigEndian.Uint16(it.Val[i*2:])
	}
	return string(utf16.Decode(units)), true
}

// StrongRef decodes a single strong reference.
func (ls *LocalSet) StrongRef(name string) (StrongRef, bool) {
	it, ok := ls.Get(name)
	if !ok || len(it.Val) != 52 {
		return StrongRef{}, false
	}
	return decodeStrongRef(it.Val), true
}

// StrongRefs decodes a counted strong-reference array.
func (ls *LocalSet) StrongRefs(name string) ([]StrongRef, bool) {
	it, ok := ls.Get(name)
	if !ok || len(it.Val) < 4 {
		return nil, false
	}
	n := binary.BigEndian.Uint32(it.Val)
	if uint64(4)+uint64(n)*52 != uint64(len(it.Val)) {
		return nil, false
	}
	refs := make([]StrongRef, n)
	for i := range refs {
		refs[i] = decodeStrongRef(it.Val[4+i*52:])
	}
	return refs, true
}

// ULProperty decodes a UL-labelled property.
func (ls *LocalSet) ULProperty(name string) (UL, bool) {
	it, ok := ls.Get(name)
	if !ok || len(it.Val) != 16 {
		return UL{}, false
	}
	var u UL
	copy(u[:], it.Val)
	return u, true
}

func decodeStrongRef(b []byte) StrongRef {
	var ref StrongRef
	copy(ref.Type[:], b[0:16])
	copy(ref.UMID[:], b[16:48])
	ref.TrackID = binary.BigEndian.Uint32(b[48:52])
	return ref
}

func encodeStrongRef(ref StrongRef) []byte {
	b := make([]byte, 52)
	copy(b[0:16], ref.Type[:])
	copy(b[16:48], ref.UMID[:])
	binary.BigEndian.PutUint32(b[48:], ref.TrackID)
	return b
}

// decodeLocalSet parses the BER-length KLV local set in data.
func decodeLocalSet(key UL, fileOff uint64, length uint64, data []byte, p primer) (*LocalSet, error) {
	ls := &LocalSet{Key: key, FileOff: fileOff, Length: length}
	pos := 0
	for pos < len(data) {
		if pos+2 > len(data) {
			return nil, model.New(model.KindInvalidMedia, "probe.mxf", "truncated local-set tag")
		}
		tag := binary.BigEndian.Uint16(data[pos:])
		pos += 2
		if tag == 0 {
			// A zero item tag terminates the set; the rest is padding.
			break
		}
		size, n, err := readBER(data[pos:])
		if err != nil {
			return nil, model.New(model.KindInvalidMedia, "probe.mxf",
				fmt.Sprintf("set %s at offset %d: tag 0x%04x: %v", key.String(), fileOff, tag, err))
		}
		pos += n
		if size > uint64(len(data)-pos) {
			return nil, model.New(model.KindInvalidMedia, "probe.mxf",
				fmt.Sprintf("set %s at offset %d: tag 0x%04x declares %d bytes, %d remain",
					key.String(), fileOff, tag, size, len(data)-pos))
		}
		prop, ok := p[tag]
		if !ok {
			return nil, model.New(model.KindInvalidMedia, "probe.mxf",
				fmt.Sprintf("set %s at offset %d: tag 0x%04x is not in the primer", key.String(), fileOff, tag))
		}
		ls.Items = append(ls.Items, Item{Tag: tag, Prop: prop, Val: data[pos : pos+int(size)], Off: fileOff + uint64(pos)})
		pos += int(size)
	}
	return ls, nil
}

// readBER decodes a BER short- or long-form length.
func readBER(b []byte) (uint64, int, error) {
	if len(b) == 0 {
		return 0, 0, fmt.Errorf("missing length")
	}
	if b[0] < 0x80 {
		return uint64(b[0]), 1, nil
	}
	n := int(b[0] & 0x7f)
	if n == 0 || n > 8 || len(b) < 1+n {
		return 0, 0, fmt.Errorf("invalid BER length prefix")
	}
	var v uint64
	for _, c := range b[1 : 1+n] {
		v = v<<8 | uint64(c)
	}
	return v, 1 + n, nil
}

// encodeBER writes v using the shortest BER form.
func encodeBER(v uint64) []byte {
	if v < 0x80 {
		return []byte{byte(v)}
	}
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], v)
	i := 0
	for i < 8 && tmp[i] == 0 {
		i++
	}
	out := make([]byte, 0, 1+8-i)
	out = append(out, 0x80|byte(8-i))
	return append(out, tmp[i:]...)
}

// appendUTF16 appends s as UTF-16BE.
func appendUTF16(dst []byte, s string) []byte {
	for _, u := range utf16.Encode([]rune(s)) {
		dst = append(dst, byte(u>>8), byte(u))
	}
	return dst
}
