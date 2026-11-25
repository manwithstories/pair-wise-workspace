// Package fingerprint creates time-independent content fingerprints and
// verifies cheap-hash matches with a second sample-summary comparison.
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash/fnv"
	"sort"
)

// Window is one comparable audio/video interval. Timestamps are intentionally
// not part of the fingerprint: an offset copy must still match.
type Window struct {
	ID      string
	TrackID int
	Samples [][]byte
}

// Fingerprint contains a cheap grouping hash plus collision-resistant sample
// summaries.
type Fingerprint struct {
	WindowID      string   `json:"window_id"`
	TrackID       int      `json:"track_id"`
	CheapHash     uint64   `json:"cheap_hash"`
	Digest        string   `json:"digest_sha256"`
	SampleDigests []string `json:"sample_digests_sha256"`
	SampleCount   int      `json:"sample_count"`
	TotalBytes    int      `json:"total_bytes"`
}

// Pair records a verified duplicate relationship.
type Pair struct {
	KeptID      string `json:"kept_id"`
	DuplicateID string `json:"duplicate_id"`
	TrackID     int    `json:"duplicate_track_id"`
	CheapHash   uint64 `json:"cheap_hash"`
}

// Of computes both hashes for a window.
func Of(w Window) Fingerprint {
	h := fnv.New64a()
	aggregate := sha256.New()
	sampleDigests := make([]string, 0, len(w.Samples))
	total := 0
	for _, sample := range w.Samples {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(sample)))
		_, _ = h.Write(length[:])
		_, _ = h.Write(sample)

		sum := sha256.Sum256(sample)
		sampleDigests = append(sampleDigests, hex.EncodeToString(sum[:]))
		_, _ = aggregate.Write(sum[:])
		total += len(sample)
	}
	return Fingerprint{
		WindowID: w.ID, TrackID: w.TrackID, CheapHash: h.Sum64(),
		Digest:        hex.EncodeToString(aggregate.Sum(nil)),
		SampleDigests: sampleDigests, SampleCount: len(w.Samples), TotalBytes: total,
	}
}

// SameContent performs the required second comparison after a cheap-hash
// match. It compares every sample summary rather than trusting the hash.
func SameContent(a, b Fingerprint) bool {
	if a.SampleCount != b.SampleCount || a.TotalBytes != b.TotalBytes || a.Digest != b.Digest {
		return false
	}
	if len(a.SampleDigests) != len(b.SampleDigests) {
		return false
	}
	for i := range a.SampleDigests {
		if a.SampleDigests[i] != b.SampleDigests[i] {
			return false
		}
	}
	return true
}

// Dedupe returns one pair for every later window that is verified identical to
// the first window in deterministic ID order. Cheap-hash collisions that fail
// SameContent are ignored.
func Dedupe(windows []Window) ([]Pair, []Fingerprint) {
	fps := make([]Fingerprint, 0, len(windows))
	for _, w := range windows {
		fps = append(fps, Of(w))
	}
	byHash := map[uint64][]Fingerprint{}
	for _, fp := range fps {
		byHash[fp.CheapHash] = append(byHash[fp.CheapHash], fp)
	}

	pairs := make([]Pair, 0)
	for _, group := range byHash {
		group := append([]Fingerprint(nil), group...)
		sort.SliceStable(group, func(i, j int) bool { return group[i].WindowID < group[j].WindowID })
		for i := 0; i < len(group); i++ {
			for j := i + 1; j < len(group); j++ {
				if SameContent(group[i], group[j]) {
					pairs = append(pairs, Pair{
						KeptID: group[i].WindowID, DuplicateID: group[j].WindowID,
						TrackID: group[j].TrackID, CheapHash: group[j].CheapHash,
					})
				}
			}
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].KeptID != pairs[j].KeptID {
			return pairs[i].KeptID < pairs[j].KeptID
		}
		return pairs[i].DuplicateID < pairs[j].DuplicateID
	})
	return pairs, fps
}
