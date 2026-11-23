// Package mxf parses the subset of MXF mediasync needs: KLV framing, partition
// packs, the primer pack and the structural metadata sets that describe track
// layout and essence placement.
package mxf

import (
	"encoding/binary"
	"errors"
	"fmt"

	"mediasync/internal/model"
)

// maxKLVValue caps a single KLV value, comfortably under the 64 MiB block cap.
const maxKLVValue = 64<<20 - 1

// PartitionKind distinguishes the three partitions mediasync reads.
type PartitionKind string

const (
	HeaderPartition PartitionKind = "header"
	BodyPartition   PartitionKind = "body"
	FooterPartition PartitionKind = "footer"
)

// partitionULs maps the three SMPTE 377M partition keys to their kind.
var partitionULs = map[[16]byte]PartitionKind{
	{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x02, 0x04, 0x00}: HeaderPartition,
	{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x02, 0x04, 0x01}: BodyPartition,
	{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x02, 0x05, 0x00}: FooterPartition,
}

// UL is a 16-byte universal label.
type UL [16]byte

// String renders the UL in SMPTE 377M dotted-hex form.
func (u UL) String() string {
	return fmt.Sprintf("%02x.%02x.%02x.%02x.%02x.%02x.%02x.%02x.%02x.%02x.%02x.%02x.%02x.%02x.%02x.%02x",
		u[0], u[1], u[2], u[3], u[4], u[5], u[6], u[7],
		u[8], u[9], u[10], u[11], u[12], u[13], u[14], u[15])
}

var (
	primerUL  = UL{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x05, 0x01, 0x00}
	essenceUL = UL{0x06, 0x0e, 0x2b, 0x34, 0x01, 0x02, 0x01, 0x01, 0x0d, 0x01, 0x03, 0x01, 0x16, 0x01, 0x00, 0x00}
)

// KLV is one key-length-value packet.
type KLV struct {
	Key    UL
	Offset int64
	Length int64
	// ValueOffset is the absolute offset of Value within the input, which is
	// the coded-sample offset for essence packets.
	ValueOffset int64
	Value       []byte
}

// Partition is a parsed partition pack plus its byte range.
type Partition struct {
	Kind       PartitionKind
	Offset     int64
	Length     int64
	HeaderLen  int64
	IndexLen   int64
	BodyLen    int64
	MajorMinor [2]byte
	KAGSize    uint32
}

// Reader parses an MXF byte stream held in memory.
type Reader struct {
	data []byte
}

// NewReader wraps an in-memory MXF stream.
func NewReader(data []byte) *Reader { return &Reader{data: data} }

// Input is the parsed MXF file.
type Input struct {
	Tracks     []*model.Track
	Stats      []model.TrackStats
	Partitions []Partition
	// Warnings collects non-fatal structural oddities with offsets.
	Warnings []Warning
}

// Warning is a non-fatal parse note tied to a byte offset.
type Warning struct {
	Offset  int64
	Message string
}

// readLength decodes a BER length starting at data[pos]. It returns the value,
// the number of bytes consumed and whether the encoding was valid.
func readLength(data []byte, pos int) (uint64, int, error) {
	if pos >= len(data) {
		return 0, 0, model.NewError(model.ErrInvalidKLV, 0, int64(pos), "length byte past end of input")
	}
	b := data[pos]
	if b < 0x80 {
		return uint64(b), 1, nil
	}
	n := int(b & 0x7f)
	if n == 0 || n > 8 {
		return 0, 0, model.NewError(model.ErrInvalidKLV, 0, int64(pos), "invalid BER length prefix 0x%02x", b)
	}
	if pos+1+n > len(data) {
		return 0, 0, model.NewError(model.ErrInvalidKLV, 0, int64(pos), "BER length truncated")
	}
	var v uint64
	for i := 0; i < n; i++ {
		v = v<<8 | uint64(data[pos+1+i])
	}
	return v, 1 + n, nil
}

// ParseKLV walks the stream, returning every packet in file order.
func (r *Reader) ParseKLV() ([]KLV, []Partition, error) {
	var packets []KLV
	var parts []Partition
	pos := 0
	for pos < len(r.data) {
		if len(r.data)-pos < 17 {
			return packets, parts, model.NewError(model.ErrInvalidKLV, 0, int64(pos), "KLV key truncated (%d bytes remain)", len(r.data)-pos)
		}
		var key UL
		copy(key[:], r.data[pos:pos+16])
		length, n, err := readLength(r.data, pos+16)
		if err != nil {
			return packets, parts, err
		}
		if length > maxKLVValue {
			return packets, parts, model.NewError(model.ErrInvalidKLV, 0, int64(pos),
				"KLV length %d at packet %d exceeds %d byte limit", length, len(packets), maxKLVValue)
		}
		valueStart := pos + 16 + n
		if length > uint64(len(r.data)-valueStart) {
			return packets, parts, model.NewError(model.ErrInvalidKLV, 0, int64(pos),
				"KLV value overruns input: declared %d, %d available", length, len(r.data)-valueStart)
		}
		value := r.data[valueStart : valueStart+int(length)]
		klv := KLV{Key: key, Offset: int64(pos), Length: int64(length), ValueOffset: int64(valueStart), Value: value}
		packets = append(packets, klv)
		if kind, ok := partitionULs[key]; ok {
			p, err := parsePartition(kind, klv)
			if err != nil {
				return packets, parts, err
			}
			parts = append(parts, p)
		}
		pos = valueStart + int(length)
	}
	return packets, parts, nil
}

// parsePartition decodes the fixed partition-pack fields.
func parsePartition(kind PartitionKind, k KLV) (Partition, error) {
	// Fixed part of an SMPTE 377M partition pack is 84 bytes.
	if len(k.Value) < 84 {
		return Partition{}, model.NewError(model.ErrInvalidKLV, 0, k.Offset, "%s partition pack too short (%d bytes)", kind, len(k.Value))
	}
	p := Partition{Kind: kind, Offset: k.Offset, Length: k.Length}
	copy(p.MajorMinor[:], k.Value[0:2])
	p.KAGSize = binary.BigEndian.Uint32(k.Value[4:8])
	p.HeaderLen = int64(binary.BigEndian.Uint64(k.Value[32:40]))
	p.IndexLen = int64(binary.BigEndian.Uint64(k.Value[40:48]))
	p.BodyLen = int64(binary.BigEndian.Uint64(k.Value[56:64]))
	return p, nil
}

// primerMap resolves two-byte local tags to their property ULs.
type primerMap map[uint16]UL

// parsePrimer reads the (tag, UL) pairs from a primer pack value.
func parsePrimer(v []byte) (primerMap, error) {
	if len(v) < 4 {
		return nil, errors.New("primer pack too short")
	}
	count := binary.BigEndian.Uint32(v[0:4])
	if count > 1<<16 {
		return nil, fmt.Errorf("primer declares %d entries", count)
	}
	if uint64(4)+uint64(count)*18 > uint64(len(v)) {
		return nil, fmt.Errorf("primer table truncated: %d entries in %d bytes", count, len(v))
	}
	m := make(primerMap, count)
	for i := uint32(0); i < count; i++ {
		off := 4 + int(i)*18
		tag := binary.BigEndian.Uint16(v[off : off+2])
		var ul UL
		copy(ul[:], v[off+2:off+18])
		m[tag] = ul
	}
	return m, nil
}

// localSet iterates properties inside a metadata set value.
type localSet struct {
	m    primerMap
	data []byte
	off  int
	err  error
}

func newLocalSet(m primerMap, v []byte) *localSet { return &localSet{m: m, data: v} }

// next returns the next (tag, value) pair, or ok=false at a clean end.
func (s *localSet) next() (tag uint16, value []byte, ok bool) {
	if s.err != nil || s.off+2 > len(s.data) {
		if s.err == nil && s.off != len(s.data) {
			s.err = fmt.Errorf("%d trailing bytes in local set", len(s.data)-s.off)
		}
		return 0, nil, false
	}
	tag = binary.BigEndian.Uint16(s.data[s.off : s.off+2])
	length, n, err := readLength(s.data, s.off+2)
	if err != nil {
		s.err = err
		return 0, nil, false
	}
	start := s.off + 2 + n
	if start+int(length) > len(s.data) {
		s.err = fmt.Errorf("tag 0x%04x value overruns set", tag)
		return 0, nil, false
	}
	value = s.data[start : start+int(length)]
	s.off = start + int(length)
	return tag, value, true
}

// nested sets appear as the value of a property whose UL names another set.
func (s *localSet) all() (map[uint16][]byte, error) {
	out := make(map[uint16][]byte)
	for {
		tag, value, ok := s.next()
		if !ok {
			break
		}
		out[tag] = value
	}
	return out, s.err
}

func uint32Of(b []byte) (uint32, bool) {
	if len(b) != 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(b), true
}

func uint16Of(b []byte) (uint16, bool) {
	if len(b) != 2 {
		return 0, false
	}
	return binary.BigEndian.Uint16(b), true
}

// editRate is a rational edit rate in ticks per second.
type editRate struct {
	Num int64
	Den int32
}

func editRateOf(b []byte) (editRate, bool) {
	if len(b) != 12 {
		return editRate{}, false
	}
	num := int64(binary.BigEndian.Uint64(b[0:8]))
	den := int32(binary.BigEndian.Uint32(b[8:12]))
	if den == 0 {
		return editRate{}, false
	}
	return editRate{Num: num, Den: den}, true
}

// Rational returns the edit rate as a num/den pair usable by clock.
func (e editRate) Rational() (int64, int64) { return e.Num, int64(e.Den) }
