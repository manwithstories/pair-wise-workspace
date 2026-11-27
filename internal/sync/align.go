// Package avsync 在统一纳秒时间线上统计音画偏差，
// 检测时间戳回退但绝不静默改写时间线。
package avsync

import (
	"sort"

	"bcastqc/internal/media"
)

// TimedPoint 是参与对齐的单个时间点（一帧或一个音频块）。
type TimedPoint struct {
	Track  uint32
	TimeNS int64
	Key    bool
}

// Report 是单文件的音画对齐统计结果。
type Report struct {
	MaxAbsDiffNS int64 // 配对点的最大绝对偏差
	MeanDiffNS   int64 // 平均带符号偏差（音频相对视频）
	P95DiffNS    int64
	Backwards    int   // 时间戳回退事件数
	Negatives    int   // 负时间戳点数
	Pairs        int
}

// MaxDiffMS 便于终端展示的毫秒口径。
func (r Report) MaxDiffMS() int64 {
	return media.AbsNS(r.MaxAbsDiffNS) / 1_000_000
}

// Aligner 累积两条轨道的时间点并产出对齐报告。
type Aligner struct {
	audio []TimedPoint
	video []TimedPoint
	rep   Report
}

// New 创建空对齐器。
func New() *Aligner { return &Aligner{} }

// Add 录入一个时间点，同时检测回退与负值（仅计数上报，不修正）。
func (a *Aligner) Add(p TimedPoint) error {
	var series *[]TimedPoint
	switch p.Track {
	case media.TrackAudio:
		series = &a.audio
		if len(a.audio) > 0 && p.TimeNS < a.audio[len(a.audio)-1].TimeNS {
			a.rep.Backwards++
		}
	case media.TrackVideo:
		series = &a.video
		if len(a.video) > 0 && p.TimeNS < a.video[len(a.video)-1].TimeNS {
			a.rep.Backwards++
		}
	default:
		return media.NewError(media.StageSync, media.KindSync,
			"未知轨道号").WithTrack(p.Track)
	}
	if p.TimeNS < 0 {
		a.rep.Negatives++
	}
	*series = append(*series, p)
	return nil
}

// Compute 以视频时间点为基准，为每个视频点找最近音频点计算偏差。
// 两条轨道都必须有序；缺失音频时不伪造，仅跳过配对。
func (a *Aligner) Compute() Report {
	if len(a.audio) == 0 || len(a.video) == 0 {
		return a.rep
	}
	audioTS := make([]int64, len(a.audio))
	for i, p := range a.audio {
		audioTS[i] = p.TimeNS
	}
	var diffs []int64
	var sum int64
	for _, v := range a.video {
		idx := nearestIndex(audioTS, v.TimeNS)
		diff := audioTS[idx] - v.TimeNS // 音频相对视频的带符号偏差
		diffs = append(diffs, diff)
		sum += diff
		if ad := media.AbsNS(diff); ad > a.rep.MaxAbsDiffNS {
			a.rep.MaxAbsDiffNS = ad
		}
	}
	a.rep.Pairs = len(diffs)
	if a.rep.Pairs > 0 {
		a.rep.MeanDiffNS = sum / int64(a.rep.Pairs)
		sort.Slice(diffs, func(i, j int) bool { return media.AbsNS(diffs[i]) < media.AbsNS(diffs[j]) })
		idx := len(diffs) * 95 / 100
		if idx >= len(diffs) {
			idx = len(diffs) - 1
		}
		a.rep.P95DiffNS = media.AbsNS(diffs[idx])
	}
	return a.rep
}

// nearestIndex 返回有序切片中距离 target 最近的下标。
func nearestIndex(sorted []int64, target int64) int {
	i := sort.Search(len(sorted), func(i int) bool { return sorted[i] >= target })
	if i == 0 {
		return 0
	}
	if i == len(sorted) {
		return len(sorted) - 1
	}
	if media.AbsNS(sorted[i]-target) < media.AbsNS(target-sorted[i-1]) {
		return i
	}
	return i - 1
}

// VerifyThreshold 在偏差超阈时返回带阶段标记的统一错误。
func VerifyThreshold(r Report, maxNS int64) error {
	if r.MaxAbsDiffNS > maxNS {
		return media.NewError(media.StageSync, media.KindSync,
			"音画偏差超阈")
	}
	return nil
}
