package stream

import (
	"fmt"
	"sort"

	"mediascan/internal/fault"
)

// GOPStats summarises keyframe spacing measured in frames.
type GOPStats struct {
	Intervals    []int // frame distance between successive keyframes
	Count        int
	Min          int
	Max          int
	P50          int
	MeanX100     int // mean interval, fixed point *100
	Faults       []fault.Fault
	Unreachable  []int // frame indexes whose nearest preceding keyframe is absent
}

// AnalyseGOP measures keyframe intervals on the shared frame index axis.
// expectedGOP is the configured GOP length; tolerance is the accepted
// fractional deviation (e.g. 0.25 allows +/-25%). Missing leading
// keyframes make frames unreachable until the first keyframe appears.
func AnalyseGOP(keyframeIdx []int, totalFrames, expectedGOP int, tolerance float64) *GOPStats {
	s := &GOPStats{Min: -1, Max: -1}
	if len(keyframeIdx) == 0 {
		s.Faults = append(s.Faults, fault.Fault{
			Cat:  fault.CatGOPAnomaly,
			Ref:  "none",
			Msg:  "stream contains no keyframes",
		})
		for i := 0; i < totalFrames; i++ {
			s.Unreachable = append(s.Unreachable, i)
		}
		return s
	}
	if keyframeIdx[0] > 0 {
		for i := 0; i < keyframeIdx[0]; i++ {
			s.Unreachable = append(s.Unreachable, i)
		}
		s.Faults = append(s.Faults, fault.Fault{
			Cat:  fault.CatGOPAnomaly,
			Span: fault.Interval{Start: 0, End: int64(keyframeIdx[0])},
			Ref:  fmt.Sprintf("first-keyframe@%d", keyframeIdx[0]),
			Msg:  fmt.Sprintf("first %d frames have no preceding keyframe", keyframeIdx[0]),
		})
	}
	var sum int
	for i := 1; i < len(keyframeIdx); i++ {
		d := keyframeIdx[i] - keyframeIdx[i-1]
		s.Intervals = append(s.Intervals, d)
		sum += d
		s.Count++
		if s.Min < 0 || d < s.Min {
			s.Min = d
		}
		if d > s.Max {
			s.Max = d
		}
		dev := float64(abs(d-expectedGOP)) / float64(expectedGOP)
		if dev > tolerance {
			s.Faults = append(s.Faults, fault.Fault{
				Cat:  fault.CatGOPAnomaly,
				Span: fault.Interval{Start: int64(keyframeIdx[i-1]), End: int64(keyframeIdx[i])},
				Ref:  fmt.Sprintf("gop@%d", keyframeIdx[i]),
				Msg:  fmt.Sprintf("interval %d deviates %.0f%% from GOP %d", d, dev*100, expectedGOP),
			})
		}
	}
	if s.Count > 0 {
		s.MeanX100 = sum * 100 / s.Count
		s.P50 = percentile(s.Intervals, 50)
	}
	return s
}

func percentile(values []int, p int) int {
	cp := append([]int(nil), values...)
	sort.Ints(cp)
	if len(cp) == 0 {
		return 0
	}
	idx := (len(cp) * p) / 100
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
