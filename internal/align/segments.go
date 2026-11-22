// Package align cuts the shared timeline into segments snapped to keyframe
// boundaries and marks segments that cannot be randomly accessed.
package align

import "sort"

// Segment is one slice of the shared timeline, [StartFrame, EndFrame).
type Segment struct {
	Index        int
	StartFrame   int64
	EndFrame     int64
	RandomAccess bool        // false when the boundary keyframe was lost
	Gaps         [][2]int64  // missing frame ranges intersecting the segment
}

// Cut splits [0, totalFrames) into segments whose boundaries snap to the
// nearest keyframe around each multiple of targetFrames. lost marks frames
// whose packets were lost; a segment starting on a lost keyframe is flagged
// RandomAccess=false. gaps are missing frame ranges on the shared timeline.
func Cut(totalFrames, targetFrames int64, keyframes []int64, lost map[int64]bool, gaps [][2]int64) []Segment {
	if totalFrames <= 0 || targetFrames <= 0 || len(keyframes) == 0 {
		return nil
	}
	bounds := []int64{0}
	for t := targetFrames; t < totalFrames; t += targetFrames {
		b := nearestKeyframe(keyframes, t)
		if b > bounds[len(bounds)-1] {
			bounds = append(bounds, b)
		}
	}
	bounds = append(bounds, totalFrames)
	segs := make([]Segment, 0, len(bounds)-1)
	for i := 0; i+1 < len(bounds); i++ {
		s := Segment{
			Index:        i,
			StartFrame:   bounds[i],
			EndFrame:     bounds[i+1],
			RandomAccess: !lost[bounds[i]],
		}
		for _, g := range gaps {
			lo, hi := max64(g[0], s.StartFrame), min64(g[1], s.EndFrame)
			if lo < hi {
				s.Gaps = append(s.Gaps, [2]int64{lo, hi})
			}
		}
		segs = append(segs, s)
	}
	return segs
}

// nearestKeyframe returns the keyframe closest to t (ties go earlier).
func nearestKeyframe(keyframes []int64, t int64) int64 {
	i := sort.Search(len(keyframes), func(i int) bool { return keyframes[i] >= t })
	if i == 0 {
		return keyframes[0]
	}
	if i == len(keyframes) {
		return keyframes[len(keyframes)-1]
	}
	if t-keyframes[i-1] <= keyframes[i]-t {
		return keyframes[i-1]
	}
	return keyframes[i]
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
