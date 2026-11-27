package index

import (
	"fmt"
	"sort"

	"avrepair/internal/media"
)

// BuildOptions supplies per-segment resolution used while building.
type BuildOptions struct {
	InputHash string
	// OutputOf maps segment ID to its committed output file name.
	OutputOf func(seg *media.Segment) (string, bool)
	// SourceHashOf returns the hash of the segment bytes actually read.
	SourceHashOf func(seg *media.Segment) string
}

// Build creates the index and rejects any track whose PTS is not strictly
// increasing. Quarantined segments contribute no entries.
func Build(tl *media.Timeline, opt BuildOptions) (*Index, error) {
	idx := &Index{Version: Version, InputHash: opt.InputHash}
	for _, track := range tl.Tracks {
		summary := Track{ID: track.ID, Kind: track.Kind, Start: media.Zero}
		var last *media.Rational
		for _, seg := range track.Segments {
			out, ok := "", false
			if opt.OutputOf != nil {
				out, ok = opt.OutputOf(seg)
			}
			if seg.Quarantine != "" || !ok || out == "" || len(seg.Samples) == 0 {
				summary.Quarantine++
				continue
			}
			sync := firstSync(seg)
			if sync == nil {
				summary.Quarantine++
				continue
			}
			if last != nil && sync.PTS.Cmp(*last) <= 0 {
				return nil, fmt.Errorf("index: track %s PTS not strictly increasing at %s (%s <= %s)", track.ID, seg.ID, sync.PTS, *last)
			}
			pts := sync.PTS
			last = &pts
			entry := Entry{
				Track:      track.ID,
				Kind:       track.Kind,
				Segment:    seg.ID,
				Source:     seg.Source,
				SourceURI:  seg.URI,
				Output:     out,
				PTS:        sync.PTS,
				Duration:   seg.Duration,
				SegmentSeq: seg.Seq,
				Sync:       true,
				TimeMS:     sync.PTS.MillisRounded(),
			}
			if opt.SourceHashOf != nil {
				entry.SourceSHA = opt.SourceHashOf(seg)
			}
			idx.Entries = append(idx.Entries, entry)
			summary.Entries++
			if summary.Entries == 1 {
				summary.Start = sync.PTS
			}
			summary.End = sync.PTS.Add(seg.Duration)
		}
		idx.Tracks = append(idx.Tracks, summary)
	}
	sort.SliceStable(idx.Entries, func(i, j int) bool {
		if idx.Entries[i].Track != idx.Entries[j].Track {
			return idx.Entries[i].Track < idx.Entries[j].Track
		}
		return idx.Entries[i].PTS.Cmp(idx.Entries[j].PTS) < 0
	})
	return idx, nil
}
