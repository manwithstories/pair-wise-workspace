// Package index builds a keyframe index from trun sample flags. It indexes
// only samples the fragment explicitly marks as sync samples; it never infers
// a keyframe from timing heuristics.
package index

import (
	"sort"

	"pairwise/ingestcheck/internal/clock"
	"pairwise/ingestcheck/internal/mp4"
)

// Entry locates one sync sample by fragment sequence and run position.
type Entry struct {
	Track     uint32
	Fragment  uint32
	Sequence  uint32
	RunIndex  int
	SampleIdx int
	BaseTime  uint64
	Time      uint64 // base decode time + preceding durations
	Size      uint32
	Offset    int64
}

// Keyframe is an indexable sync sample discovered in a fragment.
type Keyframe struct {
	Entry
}

// Index is the accumulated keyframe list, ordered by time then by fragment.
type Index struct {
	entries []Entry
	seen    map[uint64]bool
	dropped int
}

// Build walks parsed fragments and records keyframes. maxEntries bounds the
// index so a stream cannot grow it without limit; overflow is reported.
func Build(trackID uint32, frags []*mp4.Fragment, maxEntries int) (*Index, error) {
	if maxEntries <= 0 {
		return nil, clock.New(clock.CodeArgument, "index", "non-positive entry bound")
	}
	idx := &Index{seen: make(map[uint64]bool)}
	for _, f := range frags {
		if f == nil || f.Mfhd == nil || f.Tfdt == nil {
			continue
		}
		time := f.BaseTime
		for ri, run := range f.Truns {
			if run == nil {
				continue
			}
			for si, s := range run.Samples {
				flags := s.Flags
				if si == 0 && run.HasFirstSampleFlag {
					// A first-sample flag word overrides the per-sample
					// flags for the very first entry, per ISO/IEC 14496-12.
					flags = run.FirstSampleFlags
				}
				if mp4.IsKeyframe(flags) {
					key := (uint64(f.Sequence) << 32) | uint64(uint32(ri)<<16|uint32(si))&0xFFFFFFFF
					if !idx.seen[key] {
						idx.seen[key] = true
						idx.entries = append(idx.entries, Entry{
							Track:     trackID,
							Fragment:  f.Sequence,
							Sequence:  f.Sequence,
							RunIndex:  ri,
							SampleIdx: si,
							BaseTime:  f.BaseTime,
							Time:      time,
							Size:      s.Size,
							Offset:    sampleOffset(run, si),
						})
					}
				}
				time += uint64(s.Duration)
				if len(idx.entries) > maxEntries {
					idx.dropped++
					idx.entries = idx.entries[:maxEntries]
				}
			}
		}
	}
	idx.sortEntries()
	return idx, nil
}

func sampleOffset(run *mp4.Trun, si int) int64 {
	if run.Box == nil {
		return 0
	}
	base := run.Box.Offset
	if run.HasDataOffset {
		base += int64(run.DataOffset)
	}
	for i := 0; i < si; i++ {
		base += int64(run.Samples[i].Size)
	}
	return base
}

func (ix *Index) sortEntries() {
	sort.Slice(ix.entries, func(i, j int) bool {
		if ix.entries[i].Time != ix.entries[j].Time {
			return ix.entries[i].Time < ix.entries[j].Time
		}
		if ix.entries[i].Sequence != ix.entries[j].Sequence {
			return ix.entries[i].Sequence < ix.entries[j].Sequence
		}
		if ix.entries[i].RunIndex != ix.entries[j].RunIndex {
			return ix.entries[i].RunIndex < ix.entries[j].RunIndex
		}
		return ix.entries[i].SampleIdx < ix.entries[j].SampleIdx
	})
}

// Len reports the number of indexed keyframes.
func (ix *Index) Len() int { return len(ix.entries) }

// Entries returns a copy of the index.
func (ix *Index) Entries() []Entry { return append([]Entry(nil), ix.entries...) }

// Lookup returns the last keyframe at or before a media tick, and whether it
// was found. This is the "keyframe_hit" probe used by the demo.
func (ix *Index) Lookup(trackID uint32, tick uint64) (Entry, bool) {
	var best Entry
	found := false
	for _, e := range ix.entries {
		if e.Track != trackID {
			continue
		}
		if e.Time <= tick {
			best = e
			found = true
		} else {
			break
		}
	}
	return best, found
}

// Hits reports how many query ticks resolve to some keyframe.
func (ix *Index) Hits(trackID uint32, ticks []uint64) int {
	hits := 0
	for _, t := range ticks {
		if _, ok := ix.Lookup(trackID, t); ok {
			hits++
		}
	}
	return hits
}

// StableHash folds the index into a deterministic digest helper.
func (ix *Index) Fingerprint() uint64 {
	var h uint64 = 1469598103934665603
	mix := func(v uint64) { h ^= v; h *= 1099511628211 }
	for _, e := range ix.entries {
		mix(uint64(e.Sequence))
		mix(uint64(e.RunIndex))
		mix(uint64(e.SampleIdx))
		mix(e.Time)
		mix(uint64(e.Size))
		mix(uint64(e.Offset))
	}
	return h
}
