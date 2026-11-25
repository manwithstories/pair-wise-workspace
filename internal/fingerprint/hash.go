// Package fingerprint normalizes media blocks and performs collision-safe dedup.
package fingerprint

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"

	"mediaproc/internal/demux"
)

// Fingerprint is a content hash plus the exact payload needed to resolve
// theoretical or engineered SHA-256 collisions.
type Fingerprint struct {
	Hash    string
	Payload []byte
}

// Sum returns the fingerprint of one block after removing timestamp, track
// number and source offset.
func Sum(b *demux.Block) Fingerprint {
	h := sha256.New()
	codec := ""
	trackType := 0
	if b != nil && b.Track != nil {
		codec = b.Track.CodecID
		trackType = b.Track.Type
	}
	_, _ = h.Write([]byte(codec))
	_, _ = h.Write([]byte{byte(trackType), b.Lacing, boolByte(b.Keyframe)})

	var payload []byte
	if b != nil {
		for _, f := range b.Frames {
			var lb [4]byte
			lb[0] = byte(len(f) >> 24)
			lb[1] = byte(len(f) >> 16)
			lb[2] = byte(len(f) >> 8)
			lb[3] = byte(len(f))
			_, _ = h.Write(lb[:])
			_, _ = h.Write(f)
			payload = append(payload, f...)
		}
	}
	return Fingerprint{Hash: hex.EncodeToString(h.Sum(nil)), Payload: payload}
}

type entry struct {
	hash    string
	payload []byte
}

// Store remembers fingerprints and only treats equal hashes as duplicates when
// their exact payload bytes also match.
type Store struct {
	entries    map[string]entry
	Duplicates int
	Collisions int
}

// NewStore creates an empty collision-safe deduplication store.
func NewStore() *Store {
	return &Store{entries: map[string]entry{}}
}

// Check returns true only for a hash-and-payload duplicate. Equal hashes with
// different payloads are counted as collisions and retained.
func (s *Store) Check(fp Fingerprint) bool {
	old, ok := s.entries[fp.Hash]
	if !ok {
		s.entries[fp.Hash] = entry{hash: fp.Hash, payload: append([]byte(nil), fp.Payload...)}
		return false
	}
	if bytes.Equal(old.payload, fp.Payload) {
		s.Duplicates++
		return true
	}
	s.Collisions++
	return false
}

// DedupSegment removes duplicate blocks in place. It returns the number
// removed. Keyframes are never removed even if a payload repeats.
func (s *Store) DedupSegment(blocks []*demux.Block) ([]*demux.Block, int) {
	kept := blocks[:0]
	removed := 0
	for _, b := range blocks {
		if b != nil && !b.Keyframe && s.Check(Sum(b)) {
			removed++
			continue
		}
		if b != nil {
			// Insert/remember keyframes too so later identical non-keyframes can
			// be deduplicated against them.
			fp := Sum(b)
			if _, exists := s.entries[fp.Hash]; !exists {
				s.Check(fp)
			}
		}
		kept = append(kept, b)
	}
	return kept, removed
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}
