// Package index builds deterministic keyframe indexes.
package index

import (
	"sort"

	"medialab/internal/clock"
	"medialab/internal/format"
)

// Entry is one sync sample / keyframe.
type Entry struct {
	Sample    uint32
	Order     int
	RawOffset int64
	PTS       clock.Time
	Verified  bool
	Segment   int
	SHA256    string
}

// Index is a sorted, bounded keyframe index for one track.
type Index struct {
	TrackID  uint32
	Entries  []Entry
	bySample map[uint32]int
}

// MaxIndexEntries bounds in-memory keyframe indexes.
const MaxIndexEntries = 100_000

// Build creates an index from parsed stss/MXF keyframe sample numbers. Sample
// numbers are 1-based; frameDurationMS supplies the deterministic demo time.
func Build(track *format.Track, frameDurationMS int64) *Index {
	idx := &Index{TrackID: track.ID, bySample: make(map[uint32]int)}
	samples := append([]uint32(nil), track.Keyframes...)
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	if len(samples) > MaxIndexEntries {
		samples = samples[:MaxIndexEntries]
	}
	for order, sample := range samples {
		e := Entry{
			Sample:    sample,
			Order:     order,
			RawOffset: track.DataOffset,
			PTS:       clock.FromMilliseconds(int64(sample-1) * frameDurationMS),
			Segment:   int(sample - 1),
		}
		idx.bySample[sample] = len(idx.Entries)
		idx.Entries = append(idx.Entries, e)
	}
	return idx
}

// Len returns keyframe count.
func (i *Index) Len() int { return len(i.Entries) }

// Lookup returns the indexed keyframe for an exact sample number.
func (i *Index) Lookup(sample uint32) (Entry, bool) {
	pos, ok := i.bySample[sample]
	if !ok {
		return Entry{}, false
	}
	return i.Entries[pos], true
}

// MarkVerified records a hash-verified keyframe.
func (i *Index) MarkVerified(sample uint32, sum string) bool {
	pos, ok := i.bySample[sample]
	if !ok {
		return false
	}
	i.Entries[pos].Verified = true
	i.Entries[pos].SHA256 = sum
	return true
}

// LatestVerifiedBefore returns the greatest verified keyframe whose sample is
// at or before sample. This is the resume point after unrecoverable loss.
func (i *Index) LatestVerifiedBefore(sample uint32) (Entry, bool) {
	var found Entry
	ok := false
	for _, e := range i.Entries {
		if e.Sample <= sample && e.Verified {
			found, ok = e, true
		}
	}
	return found, ok
}

// HitRate computes exact sample lookup hits over a query list.
func HitRate(i *Index, samples []uint32) (hits, total int) {
	for _, s := range samples {
		total++
		if _, ok := i.Lookup(s); ok {
			hits++
		}
	}
	return hits, total
}
