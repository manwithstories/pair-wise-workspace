// Package core 实现媒体处理内核：时间线归一化、定点音频处理与再封装。
// 所有子系统共享 internal/model 的有理数时基与结构化错误。
package core

import (
	"fmt"

	"mediacheck/internal/model"
)

// RegressionPolicy 决定 tfdt 回退的处理方式，禁止静默丢弃。
type RegressionPolicy string

const (
	// RegressionReject：检测到回退即返回 ErrTFDTRegression，整组失败。
	RegressionReject RegressionPolicy = "reject"
	// RegressionRebase：以前一段结束时间重排回退分片（可复核、确定性）。
	RegressionRebase RegressionPolicy = "rebase"
)

// Gap 记录一次 mfhd 序号缺口，携带缺失序号区间与来源定位。
type Gap struct {
	MissingFrom uint32
	MissingTo   uint32
	AfterSeq    uint32
	Source      model.Interval
}

// ClockReport 是时间线归一化的可复核结果。
type ClockReport struct {
	Ordered    []model.Fragment
	Gaps       []Gap
	Rebases    []model.Interval // 发生 tfdt 重排的来源区间
	Duplicates []uint32         // 按序号+内容摘要去重的分片序号
	Conflicts  []uint32         // 同序号但内容摘要冲突 → 整组失败
	Timeline   []model.Interval // 归一化后连续区间（统一时基）
}

// NormalizeTimeline 对分片排序、去重、检测缺口并按策略处理 tfdt 回退。
// 输入的结构化错误（缺 mfhd、截断等）原样透传，不在这里吞掉。
func NormalizeTimeline(frags []model.Fragment, policy RegressionPolicy) (*ClockReport, error) {
	rep := &ClockReport{}
	if len(frags) == 0 {
		return rep, &model.MediaError{Category: model.ErrShortDuration, Message: "no fragments"}
	}
	// 1) 按序号稳定排序（副本，不改入参）。
	ordered := make([]model.Fragment, len(frags))
	copy(ordered, frags)
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && ordered[j-1].Sequence > ordered[j].Sequence; j-- {
			ordered[j-1], ordered[j] = ordered[j], ordered[j-1]
		}
	}
	// 2) 去重：同序号且内容摘要一致 → 丢弃重复；摘要冲突 → 整组失败。
	dedup := make([]model.Fragment, 0, len(ordered))
	seen := map[uint32]string{}
	for _, f := range ordered {
		if prev, ok := seen[f.Sequence]; ok {
			if prev == f.ContentSHA {
				rep.Duplicates = append(rep.Duplicates, f.Sequence)
				continue
			}
			rep.Conflicts = append(rep.Conflicts, f.Sequence)
			return rep, &model.MediaError{
				Category: model.ErrDuplicateConflict,
				Message:  fmt.Sprintf("fragment %d content hash conflict", f.Sequence),
				Source:    intervalOf(f),
			}
		}
		seen[f.Sequence] = f.ContentSHA
		dedup = append(dedup, f)
	}
	rep.Ordered = dedup
	// 3) mfhd 序号缺口检测。
	for i := 1; i < len(dedup); i++ {
		prev, cur := dedup[i-1], dedup[i]
		if cur.Sequence > prev.Sequence+1 {
			rep.Gaps = append(rep.Gaps, Gap{
				MissingFrom: prev.Sequence + 1,
				MissingTo:   cur.Sequence - 1,
				AfterSeq:    prev.Sequence,
				Source:      *intervalOf(prev),
			})
		}
	}
	// 4) tfdt 回退检测与统一时基区间构建。
	var timeline []model.Interval
	var prevEnd model.Rational
	for i, f := range dedup {
		start := model.FromTimescale(f.BaseMediaDecodeTime, f.Timescale)
		if i > 0 && start.Cmp(prevEnd) < 0 {
			src := *intervalOf(f)
			switch policy {
			case RegressionReject:
				return rep, &model.MediaError{
					Category: model.ErrTFDTRegression,
					Message:  fmt.Sprintf("fragment %d tfdt regresses", f.Sequence),
					Source:    &src,
				}
			case RegressionRebase:
				// 以前一段结束时间重排，保持时长不变。
				start = prevEnd
				rep.Rebases = append(rep.Rebases, src)
			default:
				return rep, &model.MediaError{Category: model.ErrTFDTRegression, Message: "unknown regression policy"}
			}
		}
		dur := fragmentDuration(f)
		end := start.Add(dur)
		iv := model.Interval{
			Start: start, End: end, FragmentSeq: f.Sequence,
			ByteOffset: totalOffset(f), ByteLength: f.RawSize,
		}
		timeline = append(timeline, iv)
		prevEnd = end
	}
	rep.Timeline = timeline
	return rep, nil
}

func intervalOf(f model.Fragment) *model.Interval {
	if len(f.Intervals) > 0 {
		iv := f.Intervals[0]
		return &iv
	}
	return &model.Interval{FragmentSeq: f.Sequence}
}

func totalOffset(f model.Fragment) int64 {
	if len(f.Intervals) > 0 {
		return f.Intervals[0].ByteOffset
	}
	return 0
}

// fragmentDuration 用样本时长在轨道时基下求和，再换算为统一秒。
func fragmentDuration(f model.Fragment) model.Rational {
	var ticks int64
	for _, s := range f.Samples {
		ticks += s.Duration
	}
	if ticks == 0 && len(f.Intervals) > 0 {
		// 无样本时长信息时回退到首末区间差。
		last := f.Intervals[len(f.Intervals)-1]
		return last.End.Sub(f.Intervals[0].Start)
	}
	return model.FromTimescale(ticks, f.Timescale)
}
