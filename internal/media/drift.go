package media

import (
	"fmt"
	"sort"
)

// Anchor 锚点：本地 PTS 与参考时钟（毫秒）的对应关系。
type Anchor struct {
	PTS   int64
	RefMs float64
}

// DriftSegment 一段线性漂移校正；校正量 = OffsetMs + Slope*(pts-RefPTS)。
type DriftSegment struct {
	StartPTS int64
	EndPTS   int64 // 末段为 MaxInt64
	RefPTS   int64
	OffsetMs float64
	Slope    float64 // 校正量随刻度的变化率 ms/tick
}

// DriftCorrection 分段线性漂移校正结果。
type DriftCorrection struct {
	Segments []DriftSegment
	Rejected []Anchor // 被拒绝的异常锚点
}

// maxSlopeDev 为相邻段斜率相对中位数的最大允许偏差（ms/tick）。
// 超过则视为异常锚点并拒绝；被拒绝的锚点不参与拟合，不会污染后续段。
func FitDrift(tb Timebase, anchors []Anchor, maxSlopeDev float64) (*DriftCorrection, error) {
	if err := tb.Validate(); err != nil {
		return nil, err
	}
	if len(anchors) == 0 {
		return nil, fmt.Errorf("media: no anchors to fit drift")
	}
	kept := make([]Anchor, len(anchors))
	copy(kept, anchors)
	sort.Slice(kept, func(i, j int) bool { return kept[i].PTS < kept[j].PTS })
	for i := 1; i < len(kept); i++ {
		if kept[i].PTS == kept[i-1].PTS {
			return nil, fmt.Errorf("media: duplicate anchor pts %d", kept[i].PTS)
		}
	}
	dc := &DriftCorrection{}
	// 迭代剔除异常锚点：内部锚点两侧斜率都偏离中位数即拒绝。
	for {
		outlier := findOutlier(tb, kept, maxSlopeDev)
		if outlier < 0 || len(kept) <= 2 {
			break
		}
		dc.Rejected = append(dc.Rejected, kept[outlier])
		kept = append(kept[:outlier], kept[outlier+1:]...)
	}
	dc.Segments = buildSegments(tb, kept)
	return dc, nil
}

func offsets(tb Timebase, anchors []Anchor) []float64 {
	off := make([]float64, len(anchors))
	for i, a := range anchors {
		off[i] = a.RefMs - tb.ToMillis(a.PTS)
	}
	return off
}

func slopes(tb Timebase, anchors []Anchor) []float64 {
	off := offsets(tb, anchors)
	s := make([]float64, len(anchors)-1)
	for i := range s {
		s[i] = (off[i+1] - off[i]) / float64(anchors[i+1].PTS-anchors[i].PTS)
	}
	return s
}

// findOutlier 返回首个两侧斜率均偏离中位数的内部锚点下标，无则 -1。
func findOutlier(tb Timebase, anchors []Anchor, maxDev float64) int {
	if len(anchors) < 3 {
		return -1
	}
	s := slopes(tb, anchors)
	med := median(s)
	for i := 1; i < len(anchors)-1; i++ {
		if absF(s[i-1]-med) > maxDev && absF(s[i]-med) > maxDev {
			return i
		}
	}
	return -1
}

func buildSegments(tb Timebase, anchors []Anchor) []DriftSegment {
	off := offsets(tb, anchors)
	s := slopes(tb, anchors)
	segs := make([]DriftSegment, len(anchors))
	for i := range anchors {
		seg := DriftSegment{StartPTS: anchors[i].PTS, RefPTS: anchors[i].PTS, OffsetMs: off[i]}
		if i < len(anchors)-1 {
			seg.EndPTS = anchors[i+1].PTS
			seg.Slope = s[i]
		} else {
			seg.EndPTS = int64(^uint64(0) >> 1)
			seg.Slope = 0 // 末段不外推斜率，保持常量校正
		}
		segs[i] = seg
	}
	// 首段之前按首段斜率向参考点外推（参考点锚定，不会溢出）
	segs[0].StartPTS = -segs[len(segs)-1].EndPTS - 1
	return segs
}

// CorrectMs 返回 PTS 校正后的参考毫秒时间。
func (d *DriftCorrection) CorrectMs(tb Timebase, pts int64) float64 {
	if d == nil || len(d.Segments) == 0 {
		return tb.ToMillis(pts)
	}
	for _, seg := range d.Segments {
		if pts >= seg.StartPTS && pts < seg.EndPTS {
			return tb.ToMillis(pts) + seg.OffsetMs + seg.Slope*float64(pts-seg.RefPTS)
		}
	}
	return tb.ToMillis(pts)
}

func median(v []float64) float64 {
	s := make([]float64, len(v))
	copy(s, v)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
