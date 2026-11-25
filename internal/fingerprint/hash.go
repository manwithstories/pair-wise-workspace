// Package fingerprint computes SHA-256 fingerprints of timeline segments
// and deduplicates identical content across tracks while keeping the
// source mapping.
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"

	"mediad/internal/codec"
	"mediad/internal/timeline"
)

// HashUnits hashes one track's units inside a segment: start tick,
// duration tick and payload of each unit, in order.
func HashUnits(units []codec.AccessUnit) string {
	h := sha256.New()
	var buf [8]byte
	for _, u := range units {
		binary.BigEndian.PutUint64(buf[:], uint64(u.Start.Ticks))
		h.Write(buf[:])
		binary.BigEndian.PutUint64(buf[:], uint64(u.Dur.Ticks))
		h.Write(buf[:])
		h.Write(u.Payload)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// HashSegment combines per-track hashes (sorted by track ID) into the
// segment fingerprint printed by the verifier.
func HashSegment(seg timeline.Segment) string {
	ids := make([]string, 0, len(seg.Units))
	for id := range seg.Units {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{0})
		h.Write([]byte(HashUnits(seg.Units[id])))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Source maps deduplicated content back to its origin.
type Source struct {
	Track   string `json:"track"`
	Segment int    `json:"segment"`
}

// hashUnit hashes a single access unit: start tick, duration, payload.
func hashUnit(u codec.AccessUnit) string {
	h := sha256.New()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(u.Start.Ticks))
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], uint64(u.Dur.Ticks))
	h.Write(buf[:])
	h.Write(u.Payload)
	return hex.EncodeToString(h.Sum(nil))
}

// Dedup groups identical access units across all tracks. The same audio
// arriving on several tracks (e.g. a remote mix-minus echoed back) is
// stored once, with every source kept in the mapping.
func Dedup(segs []timeline.Segment) map[string][]Source {
	out := map[string][]Source{}
	for _, seg := range segs {
		ids := make([]string, 0, len(seg.Units))
		for id := range seg.Units {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			for _, u := range seg.Units[id] {
				h := hashUnit(u)
				src := Source{Track: id, Segment: seg.Index}
				if !containsSource(out[h], src) {
					out[h] = append(out[h], src)
				}
			}
		}
	}
	return out
}

func containsSource(list []Source, s Source) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
