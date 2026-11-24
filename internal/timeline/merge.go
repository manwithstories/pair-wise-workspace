// Package timeline merges decoded Matroska tracks onto one unified
// MediaTime axis. It enforces monotonic ordering rules, records colour
// metadata conflicts by fixed source priority and exposes a stable,
// deterministic block sequence for indexing and frame export.
package timeline

import (
	"fmt"
	"sort"

	"review/internal/fixture"
	"review/internal/media"
	"review/internal/mkv"
)

// Policy controls how strictly the merge enforces ordering.
type Policy struct {
	// RejectNegative blocks blocks carrying timestamps below the epoch.
	RejectNegative bool
	// RejectDuplicateTS rejects equal timestamps on the same track
	// instead of applying the stable track-number tie-break.
	RejectDuplicateTS bool
}

// DefaultPolicy accepts negative-free streams and tie-breaks duplicates.
func DefaultPolicy() Policy {
	return Policy{RejectNegative: true, RejectDuplicateTS: false}
}

// ColourMeta is the colour triple recorded per video source.
type ColourMeta struct {
	Track          uint32
	Matrix         uint64
	Transfer       uint64
	Primaries      uint64
	SourcePriority int // lower wins when values conflict
}

// ColourConflict records one rejected colour value with the winning source.
// Missing values never participate: only present (non-zero) fields clash.
type ColourConflict struct {
	Field       string
	WinnerTrack uint32
	WinnerValue uint64
	LoserTrack  uint32
	LoserValue  uint64
}

// Timeline is the merged, ordered result.
type Timeline struct {
	Blocks          []mkv.Block
	Tracks          []mkv.Track
	ColourWinners   map[string]ColourMeta
	ColourConflicts []ColourConflict
}

// Merge combines decoded documents (one per source file) into a single
// timeline. Documents are assigned an implicit priority by argument order
// (earlier = higher priority) for colour conflict resolution.
func Merge(docs []*mkv.Document, policy Policy) (*Timeline, error) {
	tl := &Timeline{ColourWinners: map[string]ColourMeta{}}
	var blocks []mkv.Block
	for docIdx, doc := range docs {
		tl.Tracks = append(tl.Tracks, doc.Tracks...)
		// Stamp every block with its source document so blocks sharing a
		// track number across sources stay distinguishable after merging.
		for _, b := range doc.Blocks {
			b.Source = uint32(docIdx)
			if policy.RejectNegative && b.Time < 0 {
				return nil, media.NewBoundary("timeline-merge",
					fmt.Sprintf("negative timestamp %d rejected", b.Time),
					b.Track, b.Offset, nil)
			}
			blocks = append(blocks, b)
		}
		if err := tl.resolveColour(doc.Tracks, docIdx); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(blocks, func(i, j int) bool {
		a, b := blocks[i], blocks[j]
		if a.Time != b.Time {
			return a.Time < b.Time
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		// Same time, source and track: source offset gives a total
		// deterministic order unless the policy rejects the collision.
		return a.Offset < b.Offset
	})
	if policy.RejectDuplicateTS {
		for i := 1; i < len(blocks); i++ {
			a, b := blocks[i-1], blocks[i]
			if a.Time == b.Time && a.Track == b.Track {
				return nil, media.NewBoundary("timeline-merge",
					fmt.Sprintf("duplicate timestamp %d on track %d", b.Time, b.Track),
					b.Track, b.Offset, nil)
			}
		}
	}
	tl.Blocks = blocks
	return tl, nil
}

// resolveColour applies a fixed priority: the earliest document wins, then
// the lowest track number. A field present on two sources with differing
// values is recorded as a conflict rather than averaged or invented.
func (tl *Timeline) resolveColour(tracks []mkv.Track, docIdx int) error {
	fields := []struct {
		name string
		get  func(mkv.Track) uint64
	}{
		{"matrix", func(t mkv.Track) uint64 { return t.Matrix }},
		{"transfer", func(t mkv.Track) uint64 { return t.Transfer }},
		{"primaries", func(t mkv.Track) uint64 { return t.Primaries }},
	}
	for _, tr := range tracks {
		if tr.Type != fixture.TrackTypeVideo {
			continue
		}
		priority := docIdx*1000 + int(tr.Number)
		for _, f := range fields {
			value := f.get(tr)
			if value == 0 {
				continue // absent value: never synthesised, never conflicts
			}
			meta := ColourMeta{Track: tr.Number, SourcePriority: priority}
			switch f.name {
			case "matrix":
				meta.Matrix = value
			case "transfer":
				meta.Transfer = value
			case "primaries":
				meta.Primaries = value
			}
			existing, ok := tl.ColourWinners[f.name]
			if !ok {
				tl.ColourWinners[f.name] = meta
				continue
			}
			existingValue := winnerValue(existing, f.name)
			if existingValue == value {
				continue
			}
			if priority < existing.SourcePriority {
				tl.recordConflict(f.name, meta, value, existing, existingValue)
				tl.ColourWinners[f.name] = meta
			} else {
				tl.recordConflict(f.name, existing, existingValue, meta, value)
			}
		}
	}
	return nil
}

func (tl *Timeline) recordConflict(field string, winner ColourMeta, winnerValue uint64,
	loser ColourMeta, loserValue uint64) {
	tl.ColourConflicts = append(tl.ColourConflicts, ColourConflict{
		Field: field, WinnerTrack: winner.Track, WinnerValue: winnerValue,
		LoserTrack: loser.Track, LoserValue: loserValue,
	})
}

func winnerValue(m ColourMeta, field string) uint64 {
	switch field {
	case "matrix":
		return m.Matrix
	case "transfer":
		return m.Transfer
	default:
		return m.Primaries
	}
}
