// Package cut 依据剪辑点在归一时间线上切分片段，并把转场重叠表达为
// 复合段：重叠区间只计时一次，保留各组成片段的来源。
//
// 设计约束：
//   - 剪辑点落在帧/采样中间时按可配置 Rounding 吸附到该轨刻度，误差留痕；
//   - 转场重叠输出 Composite 段，绝不重复计时；
//   - 非法剪辑点（越界、倒置）报 KindInvalidArgument，不产生任何片段；
//   - 重复转场声明按 (track,start,end) 去重并计数。
package cut

import (
	"fmt"

	"archivemedia/internal/model"
	"archivemedia/internal/timeline"
)

// Kind 标记片段形态。
type Kind int

const (
	KindPlain Kind = iota
	KindTransition // 转场复合段
)

// CutPoint 是一个请求的剪辑边界（统一轴微秒时间）。
type CutPoint struct {
	At   model.MediaTime
	Mode model.Rounding
}

// Range 是一条切分请求。
type Range struct {
	TrackIndex int
	Start      CutPoint
	End        CutPoint
}

// TransitionSpec 声明两轨在 [Start,End) 的转场重叠。
type TransitionSpec struct {
	TrackA int
	TrackB int
	Start  model.MediaTime
	End    model.MediaTime
	Mode   model.Rounding
}

// RoundingRecord 记录一次吸附的输入、结果与误差。
type RoundingRecord struct {
	Requested model.MediaTime
	Snapped   model.MediaTime
	ErrorUs   int64 // snap - requested
	Mode      model.Rounding
}

// Segment 是切分产物。
type Segment struct {
	ID         string
	TrackIndex int
	Start      model.MediaTime
	End        model.MediaTime
	Duration   model.Duration
	Kind       Kind
	// Members 仅复合段使用：组成该转场的轨道片段。
	Members []SegmentMember
	// Roundings 留存本片段全部边界的吸附误差。
	Roundings []RoundingRecord
	// TransitionDedup 标记该转场是否由重复声明去重得到。
	TransitionDedup int
}

// SegmentMember 是复合段的一个组成部分。
type SegmentMember struct {
	TrackIndex int
	Start      model.MediaTime
	End        model.MediaTime
}

// Engine 执行切分。
type Engine struct {
	tl *timeline.Timeline
}

// NewEngine 绑定一条已归一的时间线。
func NewEngine(tl *timeline.Timeline) *Engine { return &Engine{tl: tl} }

// timeBase 取轨时基；越界由调用方保证。
func (e *Engine) timeBase(track int) model.Rational {
	return e.tl.Tracks[track].Track.TimeBase
}

// Cut 按 ranges 切分普通片段。所有请求先整体校验，任一非法则整体失败。
func (e *Engine) Cut(ranges []Range) ([]Segment, error) {
	for i, r := range ranges {
		if r.TrackIndex < 0 || r.TrackIndex >= len(e.tl.Tracks) {
			return nil, model.NewError(model.KindInvalidArgument,
				"cut: range %d references unknown track %d", i, r.TrackIndex)
		}
		if r.End.At <= r.Start.At {
			return nil, model.NewError(model.KindInvalidArgument,
				"cut: range %d end %d not after start %d", i, r.End.At, r.Start.At)
		}
		tr := e.tl.Tracks[r.TrackIndex]
		if len(tr.Frames) == 0 {
			return nil, model.NewError(model.KindInvalidArgument,
				"cut: track %d has no frames", r.TrackIndex)
		}
		if r.Start.At < tr.Frames[0].Start || r.End.At > tr.Frames[len(tr.Frames)-1].End {
			return nil, model.NewError(model.KindInvalidArgument,
				"cut: range %d [%d,%d) outside track span [%d,%d]",
				i, r.Start.At, r.End.At, tr.Frames[0].Start, tr.Frames[len(tr.Frames)-1].End)
		}
	}
	segs := make([]Segment, 0, len(ranges))
	for i, r := range ranges {
		tb := e.timeBase(r.TrackIndex)
		snapStart, errStart := model.SnapToTicks(r.Start.At, tb, r.Start.Mode)
		snapEnd, errEnd := model.SnapToTicks(r.End.At, tb, r.End.Mode)
		if snapEnd <= snapStart {
			return nil, model.NewError(model.KindInvalidArgument,
				"cut: range %d collapses after rounding", i)
		}
		segs = append(segs, Segment{
			ID:         fmt.Sprintf("seg-%03d", i+1),
			TrackIndex: r.TrackIndex,
			Start:      snapStart,
			End:        snapEnd,
			Duration:   model.DurationOf(snapStart, snapEnd),
			Kind:       KindPlain,
			Roundings: []RoundingRecord{
				{r.Start.At, snapStart, errStart.Microseconds(), r.Start.Mode},
				{r.End.At, snapEnd, errEnd.Microseconds(), r.End.Mode},
			},
		})
	}
	return segs, nil
}

// ComposeTransitions 把转场声明转换为复合段。
// 重复声明（同两轨同区间）去重并在 TransitionDedup 记录出现次数。
func (e *Engine) ComposeTransitions(specs []TransitionSpec) ([]Segment, error) {
	type key struct {
		a, b     int
		s, e     int64
	}
	seen := map[key]int{} // key -> 结果段在 ordered 中的下标
	var out []Segment
	for i, sp := range specs {
		if sp.TrackA < 0 || sp.TrackA >= len(e.tl.Tracks) || sp.TrackB < 0 || sp.TrackB >= len(e.tl.Tracks) {
			return nil, model.NewError(model.KindInvalidArgument,
				"transition %d references unknown track", i)
		}
		if sp.End <= sp.Start {
			return nil, model.NewError(model.KindInvalidArgument,
				"transition %d has empty/inverted span", i)
		}
		k := key{sp.TrackA, sp.TrackB, int64(sp.Start), int64(sp.End)}
		if idx, ok := seen[k]; ok {
			out[idx].TransitionDedup++ // 重复转场：去重计数，不再次计时
			continue
		}
		// 两轨各自吸附边界，复合段覆盖两轨吸附结果的并集，且只计时一次。
		tbA, tbB := e.timeBase(sp.TrackA), e.timeBase(sp.TrackB)
		aStart, aErr := model.SnapToTicks(sp.Start, tbA, sp.Mode)
		aEnd, aErr2 := model.SnapToTicks(sp.End, tbA, sp.Mode)
		bStart, bErr := model.SnapToTicks(sp.Start, tbB, sp.Mode)
		bEnd, bErr2 := model.SnapToTicks(sp.End, tbB, sp.Mode)
		unionStart := model.MinTime(aStart, bStart)
		unionEnd := model.MaxTime(aEnd, bEnd)
		seg := Segment{
			ID:         fmt.Sprintf("trans-%03d", len(out)+1),
			Start:      unionStart,
			End:        unionEnd,
			Duration:   model.DurationOf(unionStart, unionEnd), // 并集仅计一次
			Kind:       KindTransition,
			Members: []SegmentMember{
				{sp.TrackA, aStart, aEnd},
				{sp.TrackB, bStart, bEnd},
			},
			Roundings: []RoundingRecord{
				{sp.Start, aStart, aErr.Microseconds(), sp.Mode},
				{sp.End, aEnd, aErr2.Microseconds(), sp.Mode},
				{sp.Start, bStart, bErr.Microseconds(), sp.Mode},
				{sp.End, bEnd, bErr2.Microseconds(), sp.Mode},
			},
		}
		seen[k] = len(out)
		out = append(out, seg)
	}
	return out, nil
}

// TotalTimedMicros 审计用：返回全部片段计时之和，复合段只算一次，
// 用于验证转场没有重复计时。
func TotalTimedMicros(segs []Segment) int64 {
	var sum int64
	for _, s := range segs {
		sum += s.Duration.Microseconds()
	}
	return sum
}
