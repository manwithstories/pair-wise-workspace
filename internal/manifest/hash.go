// Package manifest computes per-segment hashes and defines the output
// manifest entries comparing source and remuxed target.
package manifest

import (
	"crypto/sha256"
	"encoding/binary"

	"media-check/internal/model"
)

// HashFrames hashes frames canonically: seq, unified time, keyframe flag
// and payload per frame. Deterministic across runs.
func HashFrames(frames []model.Frame) [32]byte {
	h := sha256.New()
	var buf [13]byte
	for _, f := range frames {
		binary.BigEndian.PutUint32(buf[0:4], f.Seq)
		binary.BigEndian.PutUint64(buf[4:12], uint64(f.Time))
		if f.Keyframe {
			buf[12] = 1
		} else {
			buf[12] = 0
		}
		h.Write(buf[:])
		h.Write(f.Payload)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// HashBytes is the plain SHA-256 of a byte slice.
func HashBytes(b []byte) [32]byte { return sha256.Sum256(b) }

// Segment status values in the output manifest.
const (
	StatusOK       = "ok"
	StatusCorrupt  = "corrupt"
	StatusIsolated = "isolated"
)

// SegmentEntry is one manifest line: source vs target hash and range.
type SegmentEntry struct {
	Index    int
	SrcHash  [32]byte
	DstHash  [32]byte // zero when no target was produced
	SrcRange model.TimeRange
	DstRange model.TimeRange
	Status   string
	Reason   string
}

// HashMatch reports whether source and target hashes agree.
func (e SegmentEntry) HashMatch() bool { return e.SrcHash == e.DstHash }
