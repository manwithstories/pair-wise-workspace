// Package timeline merges per-track packet sequences into one
// deterministic order independent of worker count or rerun count.
package timeline

import (
	"sort"

	"archive/internal/media"
)

// Merge concatenates the given per-track packet slices and sorts them by
// (PTS, Track, Seq). The comparator is total for distinct packets, so the
// result is identical for any worker count and any number of reruns.
// Lost-gap placeholders keep their positions; nothing is shifted.
func Merge(tracks ...[]*media.Packet) []*media.Packet {
	var all []*media.Packet
	for _, t := range tracks {
		all = append(all, t...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.PTS != b.PTS {
			return a.PTS < b.PTS
		}
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		return a.Seq < b.Seq
	})
	return all
}
