// Package index builds and validates a keyframe index over parsed tracks.
// Every entry is checked against its sample offset and track ownership; an
// index that fails validation is invalidated and rebuilt by scanning.
package index

import (
	"fmt"
	"sort"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
)

// Entry is one random-access point.
type Entry struct {
	TrackID   uint32
	SampleIdx int
	Offset    int64
	Size      int64
	DTS90k    int64
}

// Key returns a stable identity for the entry.
func (e Entry) Key() string {
	return fmt.Sprintf("%d/%d/%d", e.TrackID, e.SampleIdx, e.Offset)
}

// Index is the keyframe table of a container.
type Index struct {
	Entries []Entry
	// Rebuilds counts how many times the index was invalidated and rescanned.
	Rebuilds int
	// Valid reports whether the last build passed validation.
	Valid  bool
	Faults *fault.List
}

// Build constructs an index by scanning samples marked as sync points.
func Build(tracks []*model.Track, data []byte) *Index {
	idx := &Index{Faults: &fault.List{}, Valid: true}
	for _, t := range tracks {
		for _, s := range t.Samples {
			if !s.Sync {
				continue
			}
			dts, err := t.DTS90k(s)
			if err != nil {
				idx.Faults.Add(fault.Wrap(t.Number, s.Offset, fault.KindCorrupt, true, err))
				continue
			}
			idx.Entries = append(idx.Entries, Entry{
				TrackID: t.ID, SampleIdx: s.Index, Offset: s.Offset, Size: s.Size, DTS90k: dts,
			})
		}
	}
	Sort(idx.Entries)
	if err := idx.Validate(tracks, data); err != nil {
		idx.Faults.Add(err)
		idx.Valid = false
		idx.Revalidate(tracks, data)
	}
	return idx
}

// Validate checks each entry against the track's sample table and the essence
// buffer. Any entry that refers to an unknown track, a non-existent sample, a
// mismatched offset/size, or bytes beyond the buffer invalidates the index.
func (idx *Index) Validate(tracks []*model.Track, data []byte) *fault.Error {
	byID := make(map[uint32]*model.Track, len(tracks))
	for _, t := range tracks {
		byID[t.ID] = t
	}
	for _, e := range idx.Entries {
		t, ok := byID[e.TrackID]
		if !ok {
			return fault.New(-1, e.Offset, fault.KindOutOfRange, true,
				"index entry %s references unknown track %d", e.Key(), e.TrackID)
		}
		s, ok := t.SampleByIndex(e.SampleIdx)
		if !ok {
			return fault.New(t.Number, e.Offset, fault.KindOutOfRange, true,
				"index entry %s references missing sample %d of %s", e.Key(), e.SampleIdx, t.Label())
		}
		if s.Offset != e.Offset || s.Size != e.Size {
			return fault.New(t.Number, e.Offset, fault.KindOutOfRange, true,
				"index entry %s disagrees with sample offset/size %d/%d", e.Key(), s.Offset, s.Size)
		}
		if !s.Sync {
			return fault.New(t.Number, e.Offset, fault.KindOutOfRange, true,
				"index entry %s points at a non-sync sample", e.Key())
		}
		if e.Offset < 0 || e.Size < 0 || e.Offset+e.Size > int64(len(data)) {
			return fault.New(t.Number, e.Offset, fault.KindOutOfRange, true,
				"index entry %s range [%d,%d) outside %d-byte buffer", e.Key(), e.Offset, e.Offset+e.Size, len(data))
		}
	}
	return nil
}

// Revalidate invalidates every unverified entry and rebuilds the table from a
// full scan of track samples.
func (idx *Index) Revalidate(tracks []*model.Track, data []byte) {
	idx.Rebuilds++
	idx.Entries = idx.Entries[:0]
	seen := map[string]bool{}
	for _, t := range tracks {
		for _, s := range t.Samples {
			if !s.Sync || s.Size == 0 {
				continue
			}
			if s.Offset < 0 || s.End() > int64(len(data)) {
				continue
			}
			dts, err := t.DTS90k(s)
			if err != nil {
				continue
			}
			e := Entry{TrackID: t.ID, SampleIdx: s.Index, Offset: s.Offset, Size: s.Size, DTS90k: dts}
			if seen[e.Key()] {
				continue
			}
			seen[e.Key()] = true
			idx.Entries = append(idx.Entries, e)
		}
	}
	Sort(idx.Entries)
	idx.Valid = idx.Validate(tracks, data) == nil
}

// Shard splits entries into at most n deterministic contiguous shards.
func (idx *Index) Shard(n int) [][]Entry {
	if n <= 0 || len(idx.Entries) == 0 {
		return nil
	}
	if n > len(idx.Entries) {
		n = len(idx.Entries)
	}
	shards := make([][]Entry, 0, n)
	size := len(idx.Entries) / n
	extra := len(idx.Entries) % n
	pos := 0
	for i := 0; i < n; i++ {
		end := pos + size
		if i < extra {
			end++
		}
		shards = append(shards, idx.Entries[pos:end])
		pos = end
	}
	return shards
}

// TrackIDs returns the sorted set of track IDs present in the index.
func (idx *Index) TrackIDs() []uint32 {
	set := map[uint32]bool{}
	for _, e := range idx.Entries {
		set[e.TrackID] = true
	}
	out := make([]uint32, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Count returns the number of index entries.
func (idx *Index) Count() int { return len(idx.Entries) }

// Sort orders entries by time, then track, then sample.
func Sort(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.DTS90k != b.DTS90k {
			return a.DTS90k < b.DTS90k
		}
		if a.TrackID != b.TrackID {
			return a.TrackID < b.TrackID
		}
		return a.SampleIdx < b.SampleIdx
	})
}
