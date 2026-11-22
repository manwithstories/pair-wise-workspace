// Package index builds and queries per-track keyframe indexes. The index is
// authoritative for cut-point snapping, so its answers must depend only on the
// track contents, never on insertion order or concurrent access patterns.
package index

import (
	"sort"

	"medialine/internal/fault"
	"medialine/internal/model"
)

// Keyframe is one sync sample on the unified timeline.
type Keyframe struct {
	SampleNumber int
	PTSUS        int64
	DurationUS   int64
}

// TrackIndex maps a track's keyframes to microsecond positions and answers
// nearest-keyframe queries in both directions.
type TrackIndex struct {
	TrackID int
	frames  []Keyframe
	starts  []int64 // parallel to frames: PTSUS, ascending
	// floor caches the last successful lookup to accelerate monotonic scans.
	floor int
}

// Build constructs a TrackIndex. Samples must already be validated; otherwise
// the first offending keyframe is reported with its track and sample offset.
func Build(t *model.Track) (*TrackIndex, error) {
	if t == nil {
		return nil, fault.New(fault.StageIndex, fault.KindBadConfig, "nil track")
	}
	idx := &TrackIndex{TrackID: t.ID}
	var prev int64
	for _, s := range t.Samples {
		if !s.Keyframe {
			continue
		}
		if n := len(idx.frames); n > 0 && s.PTSUS < prev {
			return nil, fault.New(fault.StageIndex, fault.KindBackwardsTime,
				"keyframe %d at %dus precedes previous keyframe %dus", s.Number, s.PTSUS, prev).
				At(t.ID, int64(s.Number))
		}
		prev = s.PTSUS
		idx.frames = append(idx.frames, Keyframe{SampleNumber: s.Number, PTSUS: s.PTSUS, DurationUS: s.DurationUS})
	}
	sort.SliceStable(idx.frames, func(i, j int) bool { return idx.frames[i].PTSUS < idx.frames[j].PTSUS })
	idx.starts = make([]int64, len(idx.frames))
	for i, kf := range idx.frames {
		idx.starts[i] = kf.PTSUS
	}
	return idx, nil
}

// Len reports how many keyframes are indexed.
func (t *TrackIndex) Len() int { return len(t.frames) }

// At returns the i-th keyframe.
func (t *TrackIndex) At(i int) (Keyframe, bool) {
	if i < 0 || i >= len(t.frames) {
		return Keyframe{}, false
	}
	return t.frames[i], true
}

// First returns the earliest keyframe.
func (t *TrackIndex) First() (Keyframe, bool) {
	if len(t.frames) == 0 {
		return Keyframe{}, false
	}
	return t.frames[0], true
}

// Last returns the latest keyframe.
func (t *TrackIndex) Last() (Keyframe, bool) {
	if len(t.frames) == 0 {
		return Keyframe{}, false
	}
	return t.frames[len(t.frames)-1], true
}

// Before returns the latest keyframe whose PTS is <= us.
func (t *TrackIndex) Before(us int64) (Keyframe, bool) {
	i := sort.Search(len(t.starts), func(i int) bool { return t.starts[i] > us })
	if i == 0 {
		return Keyframe{}, false
	}
	return t.frames[i-1], true
}

// After returns the earliest keyframe whose PTS is >= us.
func (t *TrackIndex) After(us int64) (Keyframe, bool) {
	i := sort.Search(len(t.starts), func(i int) bool { return t.starts[i] >= us })
	if i >= len(t.frames) {
		return Keyframe{}, false
	}
	return t.frames[i], true
}

// Exact reports the keyframe that starts exactly at us, if one exists.
func (t *TrackIndex) Exact(us int64) (Keyframe, bool) {
	i := sort.Search(len(t.starts), func(i int) bool { return t.starts[i] >= us })
	if i < len(t.frames) && t.starts[i] == us {
		return t.frames[i], true
	}
	return Keyframe{}, false
}

// SnapResult describes the effective cut after snapping.
type SnapResult struct {
	// SnapUS is the replacement boundary; equal to requested when no snap
	// happened.
	SnapUS int64
	// DeltaUS is SnapUS-requested, always <= 0 when snapping backwards.
	DeltaUS int64
	// Snapped is false when the requested time sits exactly on a keyframe.
	Snapped bool
	// Found is false when the track has no keyframe at or before requested.
	Found bool
}

// SnapBack snaps requested backwards to a keyframe, as configured for segment
// boundaries that fall mid-sample. A requested time exactly on a keyframe is
// returned unchanged.
func (t *TrackIndex) SnapBack(requested int64) SnapResult {
	if kf, ok := t.Exact(requested); ok {
		return SnapResult{SnapUS: kf.PTSUS, DeltaUS: 0, Found: true}
	}
	kf, ok := t.Before(requested)
	if !ok {
		return SnapResult{SnapUS: requested, Found: false}
	}
	return SnapResult{SnapUS: kf.PTSUS, DeltaUS: kf.PTSUS - requested, Snapped: true, Found: true}
}

// MultiIndex holds the indexes for every track and provides cross-track
// lookups used by the aligner.
type MultiIndex struct {
	byTrack map[int]*TrackIndex
	order   []int
}

// NewMulti builds a MultiIndex over the given tracks.
func NewMulti(tracks []*model.Track) (*MultiIndex, error) {
	m := &MultiIndex{byTrack: make(map[int]*TrackIndex, len(tracks))}
	for _, t := range tracks {
		idx, err := Build(t)
		if err != nil {
			return nil, err
		}
		m.byTrack[t.ID] = idx
		m.order = append(m.order, t.ID)
	}
	sort.Ints(m.order)
	return m, nil
}

// Track returns the index for a track ID.
func (m *MultiIndex) Track(id int) (*TrackIndex, bool) {
	idx, ok := m.byTrack[id]
	return idx, ok
}

// TrackIDs returns track IDs in ascending order.
func (m *MultiIndex) TrackIDs() []int {
	out := make([]int, len(m.order))
	copy(out, m.order)
	return out
}

// SnapAll snaps the requested boundary on every tracked representation. The
// returned map is keyed by track ID. Tracks without an earlier keyframe are
// omitted and reported through missing.
func (m *MultiIndex) SnapAll(requested int64, tracks []int) (map[int]SnapResult, []int) {
	res := make(map[int]SnapResult, len(tracks))
	var missing []int
	for _, id := range tracks {
		idx, ok := m.byTrack[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		s := idx.SnapBack(requested)
		if !s.Found {
			missing = append(missing, id)
			continue
		}
		res[id] = s
	}
	return res, missing
}
