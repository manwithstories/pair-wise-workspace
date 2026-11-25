// Package index builds and verifies addressable keyframe indexes.
package index

import (
	"sort"

	"fieldpost/internal/core"
)

// Keyframe is one seekable random-access point.
type Keyframe struct {
	SegmentID   string
	Camera      string
	TrackID     uint32
	SampleIndex int
	PTS         core.Ticks
	DTS         core.Ticks
	ByteOffset  int64
}

// Index is a deterministic sorted keyframe table.
type Index struct {
	Keyframes []Keyframe
}

// Build indexes video sync samples. Audio samples are not seek points.
func Build(seg *core.Segment) *Index {
	idx := &Index{}
	if seg == nil {
		return idx
	}
	videoIDs := map[uint32]bool{}
	for _, tr := range seg.Tracks {
		if tr.Kind == core.TrackVideo {
			videoIDs[tr.ID] = true
		}
	}
	for i, sm := range seg.Samples {
		if videoIDs[sm.TrackID] && sm.Flags&core.FlagSync != 0 {
			idx.Keyframes = append(idx.Keyframes, Keyframe{
				SegmentID: seg.ID, Camera: seg.Camera, TrackID: sm.TrackID,
				SampleIndex: i, PTS: sm.PTS, DTS: sm.DTS, ByteOffset: sm.DataOffset,
			})
		}
	}
	sort.SliceStable(idx.Keyframes, func(i, j int) bool {
		if idx.Keyframes[i].DTS != idx.Keyframes[j].DTS {
			return idx.Keyframes[i].DTS < idx.Keyframes[j].DTS
		}
		return idx.Keyframes[i].SampleIndex < idx.Keyframes[j].SampleIndex
	})
	return idx
}

// Seek returns the greatest keyframe at or before target on trackID.
func (idx *Index) Seek(trackID uint32, target core.Ticks) (Keyframe, bool) {
	var hit Keyframe
	found := false
	for _, k := range idx.Keyframes {
		if k.TrackID != trackID {
			continue
		}
		if k.DTS <= target {
			hit, found = k, true
		} else {
			break
		}
	}
	return hit, found
}

// VerifyExact confirms that every generated keyframe is addressable by its DTS.
func (idx *Index) VerifyExact() (hits, total int) {
	for _, k := range idx.Keyframes {
		total++
		got, ok := idx.Seek(k.TrackID, k.DTS)
		if ok && got.DTS == k.DTS && got.SampleIndex == k.SampleIndex {
			hits++
		}
	}
	return hits, total
}

// HitRatio returns hits/total as a unit fraction.
func HitRatio(hits, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}
