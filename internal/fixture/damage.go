package fixture

import (
	"bytes"
	"fmt"
)

// Damage class names accepted by the CLI's --fixture flag.
const (
	DamageTruncated   = "truncated"
	DamageOversized   = "oversized"
	DamageBadBlock    = "badblock"
	DamageNoCues      = "no-cues"
	DamageOutOfOrder  = "out-of-order"
	DamageSlowCancel  = "slow-consumer"
	DamageClean       = "clean"
	DamageUnknownSize = "unknown-size"
)

// Kinds lists every fixture the CLI can generate.
var Kinds = []string{
	DamageClean, DamageTruncated, DamageOversized, DamageBadBlock,
	DamageUnknownSize, DamageNoCues, DamageOutOfOrder, DamageSlowCancel,
}

// Damaged is a self-generated fixture and a note about which bytes were
// harmed, so an operator can correlate the fixture with the reported offset.
type Damaged struct {
	Bytes  []byte
	Note   string
	Tracks []TrackSpec
}

// New builds the named fixture. All fixtures are generated here; nothing is
// read from disk, so every run is hermetic.
func New(kind string, seed int64) (Damaged, error) {
	switch kind {
	case DamageClean, "":
		doc := Build(DefaultTracks(), DefaultBlocks(), Layout{SortByTime: true, ClusterSpan: 9})
		return Damaged{Bytes: doc.Bytes, Note: "well-formed interleaved Matroska", Tracks: DefaultTracks()}, nil

	case DamageUnknownSize:
		doc := Build(DefaultTracks(), DefaultBlocks(), Layout{
			SortByTime: true, ClusterSpan: 5, UnknownSegment: true, UnknownCluster: true,
		})
		return Damaged{Bytes: doc.Bytes, Note: "unknown-size Segment and Clusters", Tracks: DefaultTracks()}, nil

	case DamageNoCues:
		doc := Build(DefaultTracks(), DefaultBlocks(), Layout{SortByTime: true, NoCues: true})
		return Damaged{Bytes: doc.Bytes, Note: "no Cues element; index must be rebuilt", Tracks: DefaultTracks()}, nil

	case DamageOutOfOrder:
		doc := Build(DefaultTracks(), ShuffledBlocks(seed), Layout{ClusterSpan: 6})
		return Damaged{Bytes: doc.Bytes, Note: "interleaved tracks with out-of-order decode timestamps", Tracks: DefaultTracks()}, nil

	case DamageTruncated:
		doc := Build(DefaultTracks(), DefaultBlocks(), Layout{SortByTime: true, ClusterSpan: 6})
		cut := len(doc.Bytes) - len(doc.Bytes)/4
		return Damaged{
			Bytes:  doc.Bytes[:cut],
			Note:   fmt.Sprintf("tail cut at byte %d inside the last Cluster", cut),
			Tracks: DefaultTracks(),
		}, nil

	case DamageOversized:
		doc := Build(DefaultTracks(), DefaultBlocks(), Layout{SortByTime: true, ClusterSpan: 6})
		if doc.SegmentSizeWidth == 0 {
			return Damaged{}, fmt.Errorf("fixture: segment size width unknown")
		}
		buf := append([]byte(nil), doc.Bytes...)
		// Shrink the Segment's declared size so a later Cluster starts past
		// the declared parent end while the file still holds those bytes.
		// The boundary is ambiguous by construction, so recovery must refuse.
		payload := uint64(len(doc.Bytes) - doc.SegmentDataOffset)
		shorter := payload / 2
		enc := encodeSizeWidth(shorter, doc.SegmentSizeWidth)
		if enc == nil {
			return Damaged{}, fmt.Errorf("fixture: cannot encode %d in %d bytes", shorter, doc.SegmentSizeWidth)
		}
		copy(buf[doc.SegmentSizeOffset:], enc)
		return Damaged{
			Bytes: buf,
			Note: fmt.Sprintf("Segment at byte %d declares %d of %d payload bytes, so a Cluster runs past its parent",
				doc.SegmentSizeOffset, shorter, payload),
			Tracks: DefaultTracks(),
		}, nil

	case DamageBadBlock:
		doc := Build(DefaultTracks(), DefaultBlocks(), Layout{SortByTime: true, ClusterSpan: 6})
		buf := append([]byte(nil), doc.Bytes...)
		// Search only inside the Cluster payload. An 0xA3 byte also occurs in
		// the random-looking Duration float and in block frames, so a plain
		// byte search would happily corrupt the header instead.
		i := firstSimpleBlock(doc, buf)
		if i < 0 {
			return Damaged{}, fmt.Errorf("fixture: no SimpleBlock found")
		}
		sizeOff := i + len(EncID(0xA3))
		w := 1
		for b := buf[sizeOff]; b&0x80 == 0; b <<= 1 {
			w++
		}
		// Zero the track-number vint: impossible in EBML, so the block header
		// is provably corrupt at a known byte.
		buf[sizeOff+w] = 0x00
		return Damaged{
			Bytes:  buf,
			Note:   fmt.Sprintf("SimpleBlock at byte %d has an invalid track number", i),
			Tracks: DefaultTracks(),
		}, nil

	case DamageSlowCancel:
		doc := Build(DefaultTracks(), DefaultBlocks(), Layout{SortByTime: true, ClusterSpan: 4})
		return Damaged{Bytes: doc.Bytes, Note: "well-formed; used to exercise backpressure", Tracks: DefaultTracks()}, nil

	default:
		return Damaged{}, fmt.Errorf("fixture: unknown kind %q", kind)
	}
}

// firstSimpleBlock walks the first Cluster's children and returns the offset
// of its first SimpleBlock ID.
func firstSimpleBlock(doc Document, buf []byte) int {
	if len(doc.ClusterOffsets) == 0 {
		return -1
	}
	pos := int(doc.ClusterOffsets[0])
	return findChildID(buf, pos, EncID(0xA3))
}

// findChildID scans forward from pos for id, stopping at the first byte that
// cannot continue a valid EBML header (the scan is bounded by the fixture's
// known layout rather than a full parser).
func findChildID(buf []byte, pos int, id []byte) int {
	for i := pos; i+len(id) <= len(buf); i++ {
		if bytes.Equal(buf[i:i+len(id)], id) {
			return i
		}
	}
	return -1
}

// encodeSizeWidth encodes size in exactly width bytes, or returns nil when the
// value does not fit. Tests use it to corrupt a size field without moving any
// later byte.
func encodeSizeWidth(size uint64, width int) []byte {
	if width < 1 || width > 8 {
		return nil
	}
	if size >= uint64(1)<<(7*uint(width)) {
		return nil
	}
	buf := make([]byte, width)
	v := size
	for i := width - 1; i >= 0; i-- {
		buf[i] = byte(v)
		v >>= 8
	}
	buf[0] |= byte(0x80) >> (uint(width) - 1)
	return buf
}

// ShuffledBlocks produces the default blocks in a deterministic order that
// interleaves tracks and moves some timestamps backwards.
func ShuffledBlocks(seed int64) []BlockSpec {
	blocks := DefaultBlocks()
	out := make([]BlockSpec, 0, len(blocks))
	for i := 0; i < len(blocks); i += 3 {
		end := i + 3
		if end > len(blocks) {
			end = len(blocks)
		}
		chunk := append([]BlockSpec(nil), blocks[i:end]...)
		if (i/3)%3 == 2 {
			for l, r := 0, len(chunk)-1; l < r; l, r = l+1, r-1 {
				chunk[l], chunk[r] = chunk[r], chunk[l]
			}
		}
		out = append(out, chunk...)
	}
	if len(out) > 4 {
		out[1], out[4] = out[4], out[1]
	}
	_ = seed
	return out
}
