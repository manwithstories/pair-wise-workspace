// Package fingerprint computes content-derived segment identifiers and
// detects cross-node duplicates. The fingerprint deliberately covers only
// media payloads and codec configuration: MPEG-TS headers, adaptation fields
// and continuity counters are excluded so the same content arriving over
// different transport paths still compares equal.
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"

	"streamcut/internal/media"
)

// writeLenBytes prefixes data with its length so concatenated fields cannot
// alias: [len][bytes] is unambiguous regardless of field boundaries.
func writeLenBytes(h interface{ Write([]byte) (int, error) }, data []byte) {
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(data)))
	_, _ = h.Write(lb[:])
	_, _ = h.Write(data)
}

func writeByte(h interface{ Write([]byte) (int, error) }, b byte) {
	_, _ = h.Write([]byte{b})
}

// ConfigHash is the codec-configuration component. Two segments can only be
// duplicates when their SPS/PPS (and any equivalent audio configuration) is
// byte-identical; otherwise equal-looking slice bytes would be a false match.
type ConfigHash struct {
	VideoConfig []byte // e.g. concatenated SPS+PPS NAL units
	AudioConfig []byte // PCM layout descriptor; empty for video-only
}

// SegmentInput is the media-only view the index hashes.
type SegmentInput struct {
	Video []media.AccessUnit
	Audio []media.AccessUnit
	Cfg   ConfigHash
}

// Index stores the first fingerprint seen for each content hash. Lookups are
// content-addressed, so repeated runs of the same deterministic input yield
// the same segment IDs without any run-local state.
type Index struct {
	first map[string]string // content hash -> first segment id
	hits  int
}

// NewIndex returns an empty index.
func NewIndex() *Index {
	return &Index{first: map[string]string{}}
}

// ContentHash folds codec configuration and every access unit's kind and
// payload into one SHA-256. Timestamps are intentionally excluded: duplicate
// content replayed at a different wall-clock position (as in the fixtures)
// must still match.
func ContentHash(in SegmentInput) string {
	h := sha256.New()
	writeLenBytes(h, in.Cfg.VideoConfig)
	writeLenBytes(h, in.Cfg.AudioConfig)
	for _, au := range in.Video {
		writeByte(h, byte(au.Kind))
		writeLenBytes(h, au.Data)
	}
	for _, au := range in.Audio {
		writeByte(h, byte(au.Kind))
		writeLenBytes(h, au.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SegmentID derives the deterministic, stable identifier for a segment.
func SegmentID(in SegmentInput) string {
	sum := sha256.Sum256([]byte(ContentHash(in)))
	return "seg-" + hex.EncodeToString(sum[:10])
}

// Observe registers a segment and reports whether it duplicates an earlier
// segment. The first occurrence returns duplicate=false and becomes the
// canonical entry; an equal later segment returns duplicate=true and the
// canonical id. Different codec config prevents a match even when payloads
// coincide, because the config is inside the hash.
func (x *Index) Observe(in SegmentInput) (id string, duplicate bool, canonical string) {
	ch := ContentHash(in)
	id = "seg-" + ch[:20]
	if prior, ok := x.first[ch]; ok {
		x.hits++
		return id, true, prior
	}
	x.first[ch] = id
	return id, false, id
}

// Hits returns how many duplicate observations have been recorded.
func (x *Index) Hits() int { return x.hits }

// Known reports whether a content hash already has a canonical segment.
func (x *Index) Known(contentHash string) bool {
	_, ok := x.first[contentHash]
	return ok
}

// Canonical returns the stored id for a content hash, or "".
func (x *Index) Canonical(contentHash string) string {
	return x.first[contentHash]
}
