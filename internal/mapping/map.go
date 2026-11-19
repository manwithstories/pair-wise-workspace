// Package mapping assigns demuxed tracks to stable output positions.
// Tracks are grouped by media type, then ordered by language tag; tracks
// without a language tag fall back to source track ID order so the mapping
// stays deterministic across runs.
package mapping

import (
	"fmt"
	"sort"

	"archive/internal/mp4"
)

// MappedTrack binds a source track to its output position.
type MappedTrack struct {
	OutputIndex int    // index within its media-type group
	Type        string // "video", "audio", "subtitle", "other"
	Language    string // may be empty
	SourceID    uint32 // source track ID
	Codec       string
}

// Key is the stable output identifier, e.g. "audio:jpn#0" or "video:#0".
func (m MappedTrack) Key() string {
	return fmt.Sprintf("%s:%s#%d", m.Type, m.Language, m.OutputIndex)
}

// Map orders tracks deterministically: by type (video, audio, subtitle,
// other), then language-tagged tracks first (alphabetical), untagged
// tracks last in ascending source-ID order.
func Map(tracks []mp4.Track) []MappedTrack {
	rank := map[string]int{"video": 0, "audio": 1, "subtitle": 2, "other": 3}
	sorted := make([]mp4.Track, len(tracks))
	copy(sorted, tracks)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if rank[a.Type] != rank[b.Type] {
			return rank[a.Type] < rank[b.Type]
		}
		if (a.Language == "") != (b.Language == "") {
			return a.Language != "" // tagged before untagged
		}
		if a.Language != b.Language {
			return a.Language < b.Language
		}
		return a.ID < b.ID // stable fallback: source ID
	})
	var out []MappedTrack
	counters := map[string]int{}
	for _, t := range sorted {
		idx := counters[t.Type]
		counters[t.Type]++
		out = append(out, MappedTrack{
			OutputIndex: idx,
			Type:        t.Type,
			Language:    t.Language,
			SourceID:    t.ID,
			Codec:       t.Codec,
		})
	}
	return out
}
