package repair

import (
	"mediaflow/internal/clock"
	"mediaflow/internal/cmaf"
)

// Index entry types. A rebuilt sample index records how each entry was
// obtained so an inferred offset is never presented as trustworthy.
const (
	SourceFragment = "fragment" // offsets recomputed from a parsed trun/tfdt
	SourceInferred = "inferred" // placed from neighbouring timing only
	SourceMissing  = "missing"
)

// IndexEntry is one entry of the rebuildable sample index for a track.
type IndexEntry struct {
	TrackID  uint32 `json:"track_id"`
	Segment  int    `json:"segment"`
	Sequence uint32 `json:"sequence"`
	StartUs  int64  `json:"start_us"`
	EndUs    int64  `json:"end_us"`
	Samples  int    `json:"samples"`
	Offset   uint64 `json:"offset,omitempty"`
	Source   string `json:"source"`
	Trusted  bool   `json:"trusted"`
}

// Index is a per-track index that can be invalidated and rebuilt. Entries
// whose data was missing from the container are marked SourceMissing rather
// than filled with a guessed position.
type Index struct {
	TrackID  uint32       `json:"track_id"`
	Complete bool         `json:"complete"`
	Missing  int          `json:"missing_entries"`
	Entries  []IndexEntry `json:"entries"`
}

// BuildIndex rebuilds a track index from recovered fragment plans. Any
// uncovered window inside the recovered span becomes a missing entry; the
// index never stores an inferred offset as trusted data.
func BuildIndex(rec Recovery) Index {
	idx := Index{TrackID: rec.TrackID, Complete: true}
	plans := make([]FragmentPlan, len(rec.Fragments))
	copy(plans, rec.Fragments)
	for _, p := range plans {
		switch p.Status {
		case StatusComplete:
			idx.Entries = append(idx.Entries, IndexEntry{
				TrackID: p.TrackID, Segment: p.Segment, Sequence: p.Sequence,
				StartUs: p.StartUs, EndUs: p.EndUs, Samples: p.Samples,
				Offset: p.Offset, Source: SourceFragment, Trusted: p.Trusted,
			})
		case StatusPartial:
			idx.Entries = append(idx.Entries, IndexEntry{
				TrackID: p.TrackID, Segment: p.Segment, Sequence: p.Sequence,
				StartUs: p.StartUs, EndUs: p.EndUs, Samples: p.Samples,
				Offset: p.Offset, Source: SourceFragment, Trusted: p.Trusted,
			})
			idx.Complete = false
		case StatusDuplicate:
			// Duplicates are intentionally excluded: the first occurrence is
			// the indexed one.
		default:
			idx.Entries = append(idx.Entries, IndexEntry{
				TrackID: p.TrackID, Segment: p.Segment, Sequence: p.Sequence,
				StartUs: p.StartUs, EndUs: p.EndUs, Source: SourceMissing, Trusted: false,
			})
			idx.Missing++
			idx.Complete = false
		}
	}
	for _, m := range rec.Missing {
		idx.Entries = append(idx.Entries, IndexEntry{
			TrackID: rec.TrackID, StartUs: m.StartUs, EndUs: m.EndUs,
			Source: SourceMissing, Trusted: false,
		})
		idx.Missing++
		idx.Complete = false
	}
	sortIndex(idx.Entries)
	return idx
}

// RecomputeSegment recomputes index entries for one segment directly from the
// parsed fragment. It is the fallback when the incremental index is missing
// entries: the fresh result is used but marked as recomputed, not guessed.
func RecomputeSegment(tl *clock.Timeline, trackID uint32, seg cmaf.MediaSegment) []IndexEntry {
	var out []IndexEntry
	for _, frag := range seg.Fragments {
		samples := trackSamples(frag, trackID)
		if len(samples) == 0 {
			continue
		}
		start, trusted := fragmentStart(tl, frag, trackID)
		if !trusted {
			start = sampleExtentStart(tl, samples)
		}
		end := start
		usable := 0
		for _, sm := range samples {
			end += durationUs(tl, sm)
			if !sm.Truncated {
				usable++
			}
		}
		source := SourceFragment
		if !trusted {
			source = SourceInferred
		}
		out = append(out, IndexEntry{
			TrackID: trackID, Segment: seg.SegmentIndex, Sequence: frag.SequenceNumber,
			StartUs: start, EndUs: end, Samples: usable, Offset: frag.MoofOffset,
			Source: source, Trusted: trusted,
		})
	}
	sortIndex(out)
	return out
}

// Valid reports whether the index can be used without a rebuild.
func (idx Index) Valid() bool {
	if !idx.Complete {
		return false
	}
	for _, e := range idx.Entries {
		if e.Source == SourceMissing || !e.Trusted {
			return false
		}
	}
	return true
}

// RebuildIndex returns a usable index for a recovery. When the recovered
// index is incomplete or contains an untrusted placement, every segment is
// re-derived from its actual parsed fragments instead of trusting the
// incremental entries; the count returned is the number of segments
// recomputed. Missing windows stay explicit in the rebuilt index.
func RebuildIndex(tl *clock.Timeline, rec Recovery, segs []cmaf.MediaSegment) (Index, int) {
	if idx := BuildIndex(rec); idx.Valid() {
		return idx, 0
	}
	fresh := Index{TrackID: rec.TrackID, Complete: true}
	recomputed := 0
	for _, seg := range segs {
		entries := RecomputeSegment(tl, rec.TrackID, seg)
		for _, e := range entries {
			if e.Source == SourceInferred {
				fresh.Complete = false
			}
			fresh.Entries = append(fresh.Entries, e)
		}
		if len(entries) > 0 {
			recomputed++
		}
	}
	for _, m := range rec.Missing {
		fresh.Entries = append(fresh.Entries, IndexEntry{
			TrackID: rec.TrackID, StartUs: m.StartUs, EndUs: m.EndUs,
			Source: SourceMissing, Trusted: false,
		})
		fresh.Missing++
		fresh.Complete = false
	}
	sortIndex(fresh.Entries)
	return fresh, recomputed
}

func sortIndex(entries []IndexEntry) {
	// Insertion sort keeps this allocation-free for the small per-fragment
	// counts and stable under equal starts.
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0; j-- {
			a, b := entries[j-1], entries[j]
			if a.StartUs < b.StartUs || (a.StartUs == b.StartUs && a.Segment <= b.Segment) {
				break
			}
			entries[j-1], entries[j] = b, a
		}
	}
}
