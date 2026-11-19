package mp4

import (
	"fmt"
	"sort"

	"archivetl/internal/clock"
)

// LookupKeyAt returns the latest keyframe whose DTS is no later than t.
// It uses binary search on the validated keyframe list.
func (idx *Index) LookupKeyAt(t clock.Tick) (Sample, bool) {
	if len(idx.Keyframes) == 0 {
		return Sample{}, false
	}
	pos := sort.Search(len(idx.Keyframes), func(i int) bool {
		return idx.Samples[idx.Keyframes[i]-1].Ticks > t
	})
	// pos is the first keyframe strictly after t; the answer is the one
	// before it. Exact hits land one position further right and are included.
	if pos == 0 {
		return Sample{}, false
	}
	return idx.Samples[idx.Keyframes[pos-1]-1], true
}

// LinearScanLookup is the deliberately independent O(n) reference
// implementation used to verify the binary-search index.
func (idx *Index) LinearScanLookup(t clock.Tick) (Sample, bool) {
	var best Sample
	found := false
	for _, s := range idx.Samples {
		if s.IsKey && s.Ticks <= t {
			best, found = s, true
		}
	}
	return best, found
}

// Verify cross-checks index lookups against the linear scan at every sample
// boundary and at one tick past the end, returning the first divergence.
func (idx *Index) Verify() error {
	probes := make([]clock.Tick, 0, len(idx.Samples)+1)
	for _, s := range idx.Samples {
		probes = append(probes, s.Ticks)
	}
	probes = append(probes, idx.Duration+1)
	for _, t := range probes {
		got, gok := idx.LookupKeyAt(t)
		want, wok := idx.LinearScanLookup(t)
		if gok != wok || (gok && got.Number != want.Number) {
			return fmt.Errorf("keyframe index mismatch at tick %d: index=%v(%v) scan=%v(%v)",
				t, got.Number, gok, want.Number, wok)
		}
	}
	return nil
}
