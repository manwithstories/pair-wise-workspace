// Package index builds a queryable keyframe index over cut segments.
package index

import "sort"

// KeyframeIndex maps media-clock keyframe timestamps to segment IDs.
type KeyframeIndex struct {
	times  []int64
	segIDs []int
	sorted bool
}

// New returns an empty index.
func New() *KeyframeIndex { return &KeyframeIndex{} }

// Add registers a keyframe at media time t pointing to segment segID.
func (ix *KeyframeIndex) Add(t int64, segID int) {
	ix.times = append(ix.times, t)
	ix.segIDs = append(ix.segIDs, segID)
	ix.sorted = false
}

func (ix *KeyframeIndex) build() {
	if ix.sorted {
		return
	}
	// Paired sort keeps times and segment IDs aligned.
	type pair struct {
		t  int64
		id int
	}
	pairs := make([]pair, len(ix.times))
	for i := range ix.times {
		pairs[i] = pair{ix.times[i], ix.segIDs[i]}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].t < pairs[j].t })
	for i, p := range pairs {
		ix.times[i], ix.segIDs[i] = p.t, p.id
	}
	ix.sorted = true
}

// Floor returns the greatest keyframe timestamp ≤ t and its segment ID.
func (ix *KeyframeIndex) Floor(t int64) (kt int64, segID int, ok bool) {
	ix.build()
	i := sort.Search(len(ix.times), func(i int) bool { return ix.times[i] > t }) - 1
	if i < 0 {
		return 0, 0, false
	}
	return ix.times[i], ix.segIDs[i], true
}

// Exact reports whether a keyframe exists exactly at t.
func (ix *KeyframeIndex) Exact(t int64) (segID int, ok bool) {
	kt, id, found := ix.Floor(t)
	if !found || kt != t {
		return 0, false
	}
	return id, true
}

// Len returns the number of indexed keyframes.
func (ix *KeyframeIndex) Len() int { return len(ix.times) }

// At returns the i-th keyframe in time order.
func (ix *KeyframeIndex) At(i int) (int64, int) {
	ix.build()
	return ix.times[i], ix.segIDs[i]
}
