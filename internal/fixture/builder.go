package fixture

import (
	"encoding/binary"

	"mediapack/internal/codec"
)

// StreamSpec configures a generated elementary stream.
type StreamSpec struct {
	Codec     codec.Codec
	Seed      int64
	Frames    int
	KeyEvery  int // frame seq is a keyframe when seq%KeyEvery==0
	ExtraSize int // extra payload bytes per frame for size control
}

// Fault controls deterministic corruption injected into generated data.
type Fault struct {
	// TruncateNAL cuts the generated stream at this absolute byte offset
	// (mid-unit), reproducing a truncated NAL.
	TruncateNAL int64
	// BadHeader flips the forbidden bit of the unit at this byte offset.
	BadHeader int64
}

// BuildStream generates one elementary stream and returns the bytes plus the
// keyframe frame-sequence numbers and each keyframe's byte offset.
func BuildStream(spec StreamSpec, f Fault) (data []byte, keySeqs []int64, keyOffsets []int64, err error) {
	switch spec.Codec {
	case codec.H264:
		return buildH264(spec, f)
	case codec.H265:
		return buildH265(spec, f)
	default:
		return buildAV1(spec, f)
	}
}

// IndexEntry is one keyframe index record.
type IndexEntry struct {
	FrameSeq int64
	Offset   int64
}

// BuildIndex serializes a keyframe index with a magic/version header.
func BuildIndex(entries []IndexEntry) []byte {
	b := make([]byte, 0, len(IndexMagic)+1+len(entries)*IndexEntrySz)
	b = append(b, []byte(IndexMagic)...)
	b = append(b, IndexVersion)
	for _, e := range entries {
		var rec [IndexEntrySz]byte
		binary.BigEndian.PutUint64(rec[0:], uint64(e.FrameSeq))
		binary.BigEndian.PutUint64(rec[8:], uint64(e.Offset))
		b = append(b, rec[:]...)
	}
	return b
}

func appendUnit(dst []byte, fourByte bool, body []byte) []byte {
	var sc [4]byte
	n := startCode(sc[:], fourByte)
	dst = append(dst, sc[:n]...)
	return append(dst, body...)
}

func withTrailer(unitHeader, payload []byte) []byte {
	body := make([]byte, 0, len(unitHeader)+len(payload)+8)
	body = append(body, unitHeader...)
	body = append(body, payload...)
	return append(body, trailer(len(unitHeader)+len(payload))...)
}
