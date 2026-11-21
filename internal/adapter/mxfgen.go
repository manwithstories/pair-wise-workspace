package adapter

import (
	"encoding/binary"
	"fmt"
)

// theFileMXFSubset is the single-partition Op1a subset this project declares
// support for. Every element is length-delimited with SMPTE 377M universal
// labels and BER lengths, so skipping an unknown KLV is always possible
// without interpreting its payload.
//
//	header partition pack | primer | metadata local set
//	| body partition pack | essence KLV... | index segment
//	| footer partition pack | RIP
//
// The index segment is mandatory for timestamped delivery; when it is absent
// the adapter derives timestamps from the edit rate and records that choice.

const mxfPartitionPackBody = 88

var (
	ulHeaderPartition = [16]byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x02, 0x04, 0x00}
	ulBodyPartition   = [16]byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x02, 0x04, 0x01}
	ulFooterPartition = [16]byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x02, 0x04, 0x02}
	ulPrimer          = [16]byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x05, 0x01, 0x00}
	ulMetadataSet     = [16]byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x53, 0x01, 0x01, 0x0d, 0x01, 0x01, 0x01, 0x01, 0x01, 0x18, 0x00}
	ulIndexSegment    = [16]byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x11, 0x01, 0x00}
	ulRIP             = [16]byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01, 0x09, 0x00, 0x00}

	// Essence element keys: picture (GC) and sound (AA).
	ulEssenceGC = [12]byte{0x06, 0x0e, 0x2b, 0x34, 0x01, 0x02, 0x01, 0x01, 0x0d, 0x01, 0x03, 0x01}
	ulEssenceAA = [12]byte{0x06, 0x0e, 0x2b, 0x34, 0x01, 0x02, 0x01, 0x01, 0x0d, 0x01, 0x03, 0x02}
)

// Local set tags.
const (
	tagTrackNumber  = uint16(0x4801)
	tagEditRate     = uint16(0x4b01)
	tagEssenceType  = uint16(0x4b02)
	tagAppliedColor = uint16(0x3210) // declared subset extension: 3 uint16 + flags
)

// MXFGenerator describes a deterministic fixture in the declared subset.
type MXFGenerator struct {
	Tracks  int
	Samples int
	Seed    int64
	// TruncateAt cuts the finished file at this offset.
	TruncateAt int64
	// UnknownKLV inserts that many skippable KLVs into the metadata region.
	UnknownKLV int
	// BadEssenceLen declares an essence KLV far longer than the file.
	BadEssenceLen bool
	// NoIndex omits the index segment so timestamps come from the edit rate.
	NoIndex bool
	// TimestampWrap starts the index clock just below 2^32 so the first wrap
	// is exercised.
	TimestampWrap bool
	// AmbiguousWrap inserts a timestamp step of exactly half the wrap range.
	AmbiguousWrap bool
}

// MXFLayout is the generated prefix and derived essence placement.
type MXFLayout struct {
	Prefix        []byte
	EssenceSize   int64
	TrackIDs      []uint32
	SampleSizes   map[uint32][]int
	SampleOffsets map[uint32][]int64
	TruncateAt    int64
}

// TotalSize is the size of the completed fixture. Essence holes are part of
// the prefix buffer for this subset, so EssenceSize is informational.
func (l *MXFLayout) TotalSize() int64 { return int64(len(l.Prefix)) }

func mxfKey(prefix [12]byte, track uint32) [16]byte {
	var k [16]byte
	copy(k[:12], prefix[:])
	binary.BigEndian.PutUint32(k[12:], track)
	return k
}

func putKey(b *byteBuf, k [16]byte) { b.b = append(b.b, k[:]...) }

func putBER(b *byteBuf, n int64) {
	switch {
	case n < 0x80:
		b.u8(uint8(n))
	case n < 0x100:
		b.u8(0x81)
		b.u8(uint8(n))
	case n < 0x10000:
		b.u8(0x82)
		b.u16(uint16(n))
	case n < 0x1000000:
		b.u8(0x83)
		b.u8(uint8(n >> 16))
		b.u16(uint16(n))
	default:
		b.u8(0x84)
		b.u32(uint32(n))
	}
}

// berLen decodes a BER length. The second result is the encoded width.
func berLen(p []byte) (int64, int, bool) {
	if len(p) == 0 {
		return 0, 0, false
	}
	if p[0] < 0x80 {
		return int64(p[0]), 1, true
	}
	n := int(p[0] & 0x7f)
	if n == 0 || n > 8 || len(p) < 1+n {
		return 0, 0, false
	}
	var v int64
	for i := 0; i < n; i++ {
		v = v<<8 | int64(p[1+i])
	}
	return v, 1 + n, true
}

func mxfSampleSize(track, i int) int {
	if track%2 == 0 {
		return 800 + (i*29)%97
	}
	return 256 + (i*7)%64
}

// BuildMXF builds the complete structural prefix. Essence payloads are holes
// in the physical file and synthesized on demand by LogicalSource.
func BuildMXF(g MXFGenerator) (*MXFLayout, error) {
	if g.Tracks < 1 || g.Tracks > 8 {
		return nil, fmt.Errorf("tracks must be in 1..8")
	}
	if g.Samples < 1 {
		return nil, fmt.Errorf("samples must be positive")
	}
	if g.UnknownKLV < 0 {
		return nil, fmt.Errorf("unknown KLV count must not be negative")
	}

	l := &MXFLayout{
		SampleSizes:   make(map[uint32][]int, g.Tracks),
		SampleOffsets: make(map[uint32][]int64, g.Tracks),
	}
	b := &byteBuf{}

	partitionPack := func(key [16]byte) {
		putKey(b, key)
		putBER(b, mxfPartitionPackBody)
		b.u16(0x0103) // major 1, minor 3
		b.u32(0)      // KAG size
		b.u64(0)      // this partition
		b.u64(0)      // previous partition
		b.u64(0)      // footer partition
		b.u64(1)      // header byte count
		b.u64(0)      // index byte count
		b.u32(0)      // index SID
		b.u64(0)      // body offset
		b.u32(0)      // body SID
		b.u16(0)      // operational pattern: OP1a
		b.zeros(mxfPartitionPackBody - 2 - 4 - 8 - 8 - 8 - 8 - 8 - 4 - 8 - 4 - 2)
	}

	partitionPack(ulHeaderPartition)
	putKey(b, ulPrimer)
	putBER(b, 0)

	meta := &byteBuf{}
	for t := 0; t < g.Tracks; t++ {
		track := uint32(t + 1)
		l.TrackIDs = append(l.TrackIDs, track)
		meta.u16(tagTrackNumber)
		meta.u16(4)
		meta.u32(track)
		meta.u16(tagEditRate)
		meta.u16(8)
		if t%2 == 0 {
			meta.u32(24)
			meta.u32(1)
		} else {
			meta.u32(48000)
			meta.u32(1024)
		}
		meta.u16(tagEssenceType)
		meta.u16(1)
		if t%2 == 0 {
			meta.u8(0)
		} else {
			meta.u8(1)
		}
	}
	putKey(b, ulMetadataSet)
	putBER(b, int64(len(meta.b)))
	b.b = append(b.b, meta.b...)

	for i := 0; i < g.UnknownKLV; i++ {
		k := ulMetadataSet
		k[13] = 0x7f
		k[14] = 0x01
		putKey(b, k)
		putBER(b, 6)
		b.u32(uint32(i))
		b.u16(0)
	}

	partitionPack(ulBodyPartition)

	for i := 0; i < g.Samples; i++ {
		for t := 0; t < g.Tracks; t++ {
			track := uint32(t + 1)
			size := mxfSampleSize(t, i)
			if t%2 == 0 {
				putKey(b, mxfKey(ulEssenceGC, track))
			} else {
				putKey(b, mxfKey(ulEssenceAA, track))
			}
			declared := int64(size)
			if g.BadEssenceLen && i == 0 && t == 0 {
				declared = int64(size) * 1000
			}
			putBER(b, declared)
			// The payload offset is where this KLV's essence actually starts,
			// after its key and BER length.
			l.SampleOffsets[track] = append(l.SampleOffsets[track], int64(len(b.b)))
			l.SampleSizes[track] = append(l.SampleSizes[track], size)
			b.zeros(size)
		}
	}

	if !g.NoIndex {
		idx := &byteBuf{}
		idx.u32(uint32(g.Samples))
		clock := uint32(0)
		if g.TimestampWrap {
			clock = 0xFFFFFF00
		}
		for i := 0; i < g.Samples; i++ {
			if g.AmbiguousWrap && i == g.Samples/2 {
				clock += 1 << 31
			}
			idx.u32(clock)
			idx.u32(0)
			clock += 1001 // arbitrary but monotonic at a 30k clock
		}
		putKey(b, ulIndexSegment)
		putBER(b, int64(len(idx.b)))
		b.b = append(b.b, idx.b...)
	}

	partitionPack(ulFooterPartition)
	putKey(b, ulRIP)
	putBER(b, 4)
	b.u32(1)

	l.Prefix = b.b
	l.EssenceSize = 0
	if g.TruncateAt > 0 {
		l.TruncateAt = g.TruncateAt
	}
	return l, nil
}
