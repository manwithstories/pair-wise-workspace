// Package index builds per-track keyframe indexes over a normalized
// timeline. Indexes tolerate sparse keyframes and non-zero start times.
package index

import (
	"fmt"
	"sort"

	"medpipe/internal/clock"
	"medpipe/internal/model"
)

// Entry is one indexed keyframe on the shared nanosecond timeline.
type Entry struct {
	NS       int64
	Seq      uint64
	TrackID  string
	FrameIdx int
}

// TrackIndex is a sorted keyframe index for one track.
type TrackIndex struct {
	TrackID string
	Entries []Entry
	// StartNS/EndNS bound the track's media on the shared timeline.
	StartNS int64
	EndNS   int64
	// IntervalNS is the track's median frame interval.
	IntervalNS int64
}

// QueryResult reports the nearest keyframe at or before the target.
type QueryResult struct {
	Entry Entry
	Hit   bool
	// DeltaNS is target - entry.NS when Hit; 0 otherwise.
	DeltaNS int64
}

// Index holds one TrackIndex per track plus a global ordering.
type Index struct {
	Tracks map[string]*TrackIndex
	// All is the global keyframe sequence sorted by time, then track ID.
	All []Entry
}

// Build indexes every track of the timeline. Tracks may contain zero
// keyframes; the index is still queryable and reports misses.
func Build(tl *clock.Timeline) (*Index, error) {
	if tl == nil {
		return nil, model.New(model.KindInput, "index.Build", "nil timeline")
	}
	idx := &Index{Tracks: make(map[string]*TrackIndex, len(tl.Tracks))}
	for i := range tl.Tracks {
		tt := &tl.Tracks[i]
		if _, dup := idx.Tracks[tt.Track.ID]; dup {
			return nil, model.New(model.KindInput, "index.Build", fmt.Sprintf("duplicate track %q", tt.Track.ID))
		}
		ti := &TrackIndex{TrackID: tt.Track.ID, StartNS: tt.StartNS, EndNS: tt.EndNS, IntervalNS: tt.FrameIntervalNS}
		for j, fr := range tt.Frames {
			if !fr.Keyframe {
				continue
			}
			ti.Entries = append(ti.Entries, Entry{NS: fr.NS, Seq: fr.Seq, TrackID: tt.Track.ID, FrameIdx: j})
		}
		sort.SliceStable(ti.Entries, func(a, b int) bool { return ti.Entries[a].NS < ti.Entries[b].NS })
		for k := 1; k < len(ti.Entries); k++ {
			if ti.Entries[k].NS == ti.Entries[k-1].NS && ti.Entries[k].Seq == ti.Entries[k-1].Seq {
				return nil, model.New(model.KindInvariant, "index.Build", fmt.Sprintf("duplicate keyframe in track %s", tt.Track.ID))
			}
		}
		idx.Tracks[tt.Track.ID] = ti
		idx.All = append(idx.All, ti.Entries...)
	}
	sort.SliceStable(idx.All, func(a, b int) bool {
		if idx.All[a].NS != idx.All[b].NS {
			return idx.All[a].NS < idx.All[b].NS
		}
		return idx.All[a].TrackID < idx.All[b].TrackID
	})
	return idx, nil
}

// Query returns the closest keyframe not later than target. Tracks with a
// keyframe exactly at target report a hit with DeltaNS 0. A target before the
// first keyframe is a clean miss, not an error.
func (ti *TrackIndex) Query(targetNS int64) QueryResult {
	n := sort.Search(len(ti.Entries), func(i int) bool { return ti.Entries[i].NS > targetNS })
	if n == 0 {
		return QueryResult{}
	}
	e := ti.Entries[n-1]
	return QueryResult{Entry: e, Hit: true, DeltaNS: targetNS - e.NS}
}

// QueryTrack looks up one track by ID.
func (idx *Index) QueryTrack(trackID string, targetNS int64) (QueryResult, error) {
	ti, ok := idx.Tracks[trackID]
	if !ok {
		return QueryResult{}, model.New(model.KindNotFound, "index.QueryTrack", fmt.Sprintf("unknown track %q", trackID))
	}
	return ti.Query(targetNS), nil
}

// KeyframesBetween returns the keyframe entries with NS in the half-open
// interval [startNS, endNS) for a track.
func (ti *TrackIndex) KeyframesBetween(startNS, endNS int64) []Entry {
	lo := sort.Search(len(ti.Entries), func(i int) bool { return ti.Entries[i].NS >= startNS })
	hi := sort.Search(len(ti.Entries), func(i int) bool { return ti.Entries[i].NS >= endNS })
	if lo >= hi {
		return nil
	}
	return append([]Entry(nil), ti.Entries[lo:hi]...)
}
