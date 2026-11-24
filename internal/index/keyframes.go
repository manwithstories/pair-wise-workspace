// Package index builds a deterministic keyframe/sample index over a merged
// timeline. The index is the single source of truth for frame export and
// for the go-mp4 preview cross-check, so timing drift is caught before it
// reaches the review output.
package index

import (
	"fmt"
	"sort"

	"review/internal/media"
	"review/internal/mkv"
)

// Entry is one indexed sample.
type Entry struct {
	Ordinal  int               // 0-based position across the whole timeline
	Sample   uint32            // 1-based sample number used in MP4 tables
	Track    uint32
	Time     media.MediaTime
	Offset   int64             // source byte offset for provenance
	Keyframe bool
}

// Index is the ordered sample table for one video track selection.
type Index struct {
	Track     uint32
	Entries   []Entry
	Keyframes []Entry
}

// Build indexes every block of the requested video track in timeline
// order. Keyframe flags come from the Matroska SimpleBlock bit; blocks
// without any keyframe are rejected because frame export needs sync points.
func Build(track uint32, blocks []mkv.Block) (*Index, error) {
	idx := &Index{Track: track}
	ordinal := 0
	for _, b := range blocks {
		if b.Track != track {
			continue
		}
		e := Entry{
			Ordinal: ordinal, Sample: uint32(ordinal + 1),
			Track: b.Track, Time: b.Time, Offset: b.Offset,
			Keyframe: b.Keyframe,
		}
		idx.Entries = append(idx.Entries, e)
		if e.Keyframe {
			idx.Keyframes = append(idx.Keyframes, e)
		}
		ordinal++
	}
	if len(idx.Entries) == 0 {
		return nil, media.NewBoundary("keyframe-index",
			fmt.Sprintf("no samples found for track %d", track), track, -1, nil)
	}
	if len(idx.Keyframes) == 0 {
		return nil, media.NewBoundary("keyframe-index",
			fmt.Sprintf("track %d has no keyframe/sync samples", track), track, -1, nil)
	}
	return idx, nil
}

// KeyframeHitRate returns the fraction (0..1) of requested timestamps that
// land on an indexed keyframe within tolerance.
func (ix *Index) KeyframeHitRate(wanted []media.MediaTime, tolerance media.MediaTime) float64 {
	if len(wanted) == 0 {
		return 0
	}
	hits := 0
	keySet := map[media.MediaTime]bool{}
	for _, k := range ix.Keyframes {
		keySet[k.Time] = true
	}
	for _, w := range wanted {
		for t := range keySet {
			diff := t - w
			if diff < 0 {
				diff = -diff
			}
			if diff <= tolerance {
				hits++
				break
			}
		}
	}
	return float64(hits) / float64(len(wanted))
}

// NearestKeyframe returns the keyframe entry at or before t (the decode
// anchor an export should start from).
func (ix *Index) NearestKeyframe(t media.MediaTime) (Entry, error) {
	pos := sort.Search(len(ix.Keyframes), func(i int) bool {
		return ix.Keyframes[i].Time > t
	})
	if pos == 0 {
		return Entry{}, media.NewBoundary("keyframe-index",
			fmt.Sprintf("no keyframe at or before %d", t), ix.Track, -1, nil)
	}
	return ix.Keyframes[pos-1], nil
}

// PreviewFrames projects the index onto fixture.PreviewFrame-shaped rows
// for MP4 serialization; timestamps are converted to the requested
// timescale using exact integer scaling.
type PreviewRow struct {
	Sample     uint32
	TimeScaled uint32
	Keyframe   bool
}

// PreviewRows converts entries to a constant-delta preview table. The
// delta is the median gap expressed in timescale units.
func (ix *Index) PreviewRows(timescale uint32) ([]PreviewRow, uint32, error) {
	if timescale == 0 {
		return nil, 0, media.NewInternal("keyframe-index", "zero timescale", nil)
	}
	if len(ix.Entries) < 2 {
		return nil, 0, media.NewBoundary("keyframe-index",
			"need at least two samples to derive a delta", ix.Track, -1, nil)
	}
	deltaNS := ix.Entries[1].Time.Nanoseconds() - ix.Entries[0].Time.Nanoseconds()
	if deltaNS <= 0 {
		return nil, 0, media.NewBoundary("keyframe-index",
			"non-increasing sample timestamps", ix.Track, ix.Entries[1].Offset, nil)
	}
	delta := uint32(deltaNS * int64(timescale) / media.NanosPerSecond)
	rows := make([]PreviewRow, len(ix.Entries))
	for i, e := range ix.Entries {
		scaled := uint32(e.Time.Nanoseconds() * int64(timescale) / media.NanosPerSecond)
		rows[i] = PreviewRow{Sample: e.Sample, TimeScaled: scaled, Keyframe: e.Keyframe}
	}
	return rows, delta, nil
}
