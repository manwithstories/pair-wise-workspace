package stream

import (
	"sort"

	"mediacheck/internal/fault"
)

// Range is a closed-open interval [Start, End) on the shared ns base.
type Range struct {
	Start int64 `json:"start_ns"`
	End   int64 `json:"end_ns"`
}

// Overlap reports whether two closed-open ranges intersect.
func Overlap(a, b Range) bool { return a.Start < b.End && b.Start < a.End }

// Segment is one GOP-aligned slice of the program timeline.
type Segment struct {
	Index     int   `json:"index"`
	Start     int64 `json:"start_ns"` // closed-open
	End       int64 `json:"end_ns"`
	Keyframe  int64 `json:"keyframe_ns"`
	Corrupted bool  `json:"corrupted"`
}

// BuildSegments splits [0, duration) at keyframe boundaries and marks
// segments overlapping any corrupt range. All bounds are closed-open.
func BuildSegments(keyframes []int64, duration int64, corrupt []Range) []Segment {
	if len(keyframes) == 0 || duration <= 0 {
		return nil
	}
	ks := append([]int64(nil), keyframes...)
	sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })
	var segs []Segment
	for i, k := range ks {
		end := duration
		if i+1 < len(ks) {
			end = ks[i+1]
		}
		if end <= k {
			continue // duplicate keyframe timestamp: no segment
		}
		seg := Segment{Index: len(segs), Start: k, End: end, Keyframe: k}
		for _, c := range corrupt {
			if Overlap(Range{k, end}, c) {
				seg.Corrupted = true
				break
			}
		}
		segs = append(segs, seg)
	}
	return segs
}

// CheckGOP flags keyframe intervals exceeding maxInterval as
// recoverable stream faults (keyframe-interval anomalies).
func CheckGOP(keyframes []int64, maxInterval int64, fl *fault.List) {
	for i := 1; i < len(keyframes); i++ {
		if d := keyframes[i] - keyframes[i-1]; d > maxInterval {
			fl.Addf(fault.CatStream, keyframes[i-1], true,
				"keyframe interval %d ns exceeds max %d ns", d, maxInterval)
		}
		if keyframes[i] == keyframes[i-1] {
			fl.Addf(fault.CatStream, keyframes[i], true,
				"duplicate keyframe timestamp %d", keyframes[i])
		}
	}
}

// HitRate is the fraction of expected keyframe times matched by an
// actual keyframe within toleranceNs.
func HitRate(expected, actual []int64, toleranceNs int64) float64 {
	if len(expected) == 0 {
		return 1
	}
	hits := 0
	for _, e := range expected {
		for _, a := range actual {
			d := a - e
			if d < 0 {
				d = -d
			}
			if d <= toleranceNs {
				hits++
				break
			}
		}
	}
	return float64(hits) / float64(len(expected))
}
