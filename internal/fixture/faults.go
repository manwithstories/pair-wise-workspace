package fixture

import (
	"encoding/binary"

	"github.com/pion/rtp"
)

// BadIndexKinds enumerate the index corruption scenarios produced on demand.
type BadIndexKind int

const (
	BadIndexMagic BadIndexKind = iota
	BadIndexVersion
	BadIndexOutOfRange
	BadIndexPartialRecord
)

// BuildBadIndex serializes a deliberately invalid keyframe index.
func BuildBadIndex(kind BadIndexKind, inputSize int64) []byte {
	switch kind {
	case BadIndexMagic:
		b := BuildIndex([]IndexEntry{{FrameSeq: 0, Offset: 0}})
		copy(b[:7], []byte("BADMAG1"))
		return b
	case BadIndexVersion:
		b := BuildIndex([]IndexEntry{{FrameSeq: 0, Offset: 0}})
		b[len(IndexMagic)] = 99
		return b
	case BadIndexOutOfRange:
		return BuildIndex([]IndexEntry{{FrameSeq: 0, Offset: inputSize + 10}})
	default:
		b := BuildIndex([]IndexEntry{{FrameSeq: 0, Offset: 0}})
		return b[:len(b)-4] // partial trailing record
	}
}

// BuildDuplicateRTP reuses BuildRTP then duplicates one packet and drops a
// later one, exercising both duplicate and gap validation.
func BuildDuplicateRTP(spec RTPSpec, dupAt, dropAt int) ([][]byte, error) {
	base, err := BuildRTP(spec)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for i, p := range base {
		if i == dropAt {
			continue
		}
		out = append(out, p)
		if i == dupAt {
			out = append(out, p) // exact duplicate
		}
	}
	return out, nil
}

// BuildWrappingRTP emits sequence numbers that cross the 16-bit boundary.
func BuildWrappingRTP(ssrc uint32, count int) ([][]byte, error) {
	out := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 96,
				SequenceNumber: uint16(0x10000 - count/2 + i),
				Timestamp:      uint32(i * 3000),
				SSRC:           ssrc,
			},
			Payload: []byte{byte(i)},
		}
		raw, err := pkt.Marshal()
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

// WrappingTimestampAU builds access-unit PTS values crossing the 32-bit edge.
func WrappingTimestampAU(frames int) []uint32 {
	ts := make([]uint32, frames)
	base := uint32(0xFFFFFFFF) - uint32(frames)*1500
	for i := range ts {
		ts[i] = base + uint32(i)*3000
	}
	return ts
}

// SlowConsumerScript describes a consumer that blocks on the first fragments.
type SlowConsumerScript struct {
	BlockMs int
	After   int // fragments produced before the block begins
}

var _ = binary.BigEndian
