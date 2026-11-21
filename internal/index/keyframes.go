// Package index builds the seekable keyframe index over recovered frames and
// records the synthetic ground-truth hit rate for archive verification.
package index

import (
	"crypto/sha256"
	"encoding/hex"
)

// FrameView is the pipeline-neutral subset of one recovered video frame.
type FrameView struct {
	ID        uint32
	SSRC      uint32
	PTSUs     int64
	Keyframe  bool
	Bad       bool
	FirstTick int64
}

// Entry is one indexable keyframe location. Bad frames are never indexed even
// if they carry the keyframe flag, so a seek always lands on decodable data.
type Entry struct {
	FrameID   uint32
	SSRC      uint32
	PTSUs     int64
	FirstTick int64
	SHA256    string
}

// Index is the complete keyframe table for one restore run.
type Index struct {
	Entries []Entry
}

// KeyframeIndexer accumulates entries in presentation order.
type KeyframeIndexer struct {
	idx  Index
	seen map[uint32]bool
}

// NewKeyframeIndexer creates an empty indexer.
func NewKeyframeIndexer() *KeyframeIndexer {
	return &KeyframeIndexer{seen: map[uint32]bool{}}
}

// Add indexes a keyframe; non-keyframes and corrupted frames are skipped.
// Adding the same frame ID twice is idempotent, which a resume replay relies
// on. The entry hash covers identity and timing evidence.
func (k *KeyframeIndexer) Add(f FrameView) {
	if !f.Keyframe || f.Bad || k.seen[f.ID] {
		return
	}
	k.seen[f.ID] = true
	h := sha256.Sum256([]byte{
		byte(f.ID >> 24), byte(f.ID >> 16), byte(f.ID >> 8), byte(f.ID),
		byte(f.SSRC >> 24), byte(f.SSRC >> 16), byte(f.SSRC >> 8), byte(f.SSRC),
	})
	k.idx.Entries = append(k.idx.Entries, Entry{
		FrameID:   f.ID,
		SSRC:      f.SSRC,
		PTSUs:     f.PTSUs,
		FirstTick: f.FirstTick,
		SHA256:    hex.EncodeToString(h[:]),
	})
}

// Result returns the accumulated index.
func (k *KeyframeIndexer) Result() Index { return k.idx }

// Lookup finds the index entry at or before the requested presentation time.
// The boolean is false when no keyframe precedes ptsUs.
func (x Index) Lookup(ptsUs int64) (Entry, bool) {
	var best Entry
	found := false
	for _, e := range x.Entries {
		if e.PTSUs <= ptsUs && (!found || e.PTSUs > best.PTSUs) {
			best, found = e, true
		}
	}
	return best, found
}

// HitRate compares the built index against the synthetic ground-truth list
// of keyframe IDs: hits / total truth keyframes. Bad truth keyframes are
// expected to be absent and count as neither hit nor miss.
func (x Index) HitRate(truthKeyframeIDs []uint32, badIDs map[uint32]bool) float64 {
	have := map[uint32]bool{}
	for _, e := range x.Entries {
		have[e.FrameID] = true
	}
	total, hits := 0, 0
	for _, id := range truthKeyframeIDs {
		if badIDs[id] {
			continue
		}
		total++
		if have[id] {
			hits++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}
