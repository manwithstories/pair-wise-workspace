// Package repair 识别并修复媒体时间戳的回绕、重复、倒退与跨剪辑跳变，
// 输出统一时间基准下的单调连续时间戳，原始值保留供审计。
package repair

import (
	"fmt"

	"mediarepair/internal/fault"
	"mediarepair/internal/model"
)

// Policy 是单类异常的修复策略。
type Policy string

const (
	PolicyClamp Policy = "clamp" // 钳制为单调连续
	PolicyKeep  Policy = "keep"  // 仅标记，不修改（审计模式）
)

// Strategy 汇总各类异常的修复策略；DriftPPM 非零时按比例修正时钟漂移。
type Strategy struct {
	Wrap       Policy
	Duplicate  Policy
	Regression Policy
	ClipJump   Policy
	DriftPPM   int64
}

// DefaultStrategy 全部异常钳制修复，不做漂移修正。
func DefaultStrategy() Strategy {
	return Strategy{Wrap: PolicyClamp, Duplicate: PolicyClamp, Regression: PolicyClamp, ClipJump: PolicyClamp}
}

// TrackStats 是单轨修复统计。
type TrackStats struct {
	TrackID     uint32
	Samples     uint64
	Wraps       uint64
	Duplicates  uint64
	Regressions uint64
	ClipJumps   uint64
}

// segment 是轨道上的一段连续区间（剪辑或转场）。
type segment struct {
	id           string
	start, end   uint64
	isTransition bool
}

type trackState struct {
	timescale    uint32
	haveLast     bool
	lastRaw      uint64 // 上一次原始 DTS（未展开）
	wrapBase     uint64 // 回绕展开偏移
	nextExpected uint64 // 修复后期望的下一个 DTS（展开域）
	lastDur      uint64
	segments     []segment
	stats        TrackStats
}

// Clock 按轨道维护修复状态。
type Clock struct {
	strat  Strategy
	tl     *model.Timeline
	tracks map[uint32]*trackState
}

// NewClock 基于时间线构建修复器；时间线须先通过 Validate。
func NewClock(tl *model.Timeline, strat Strategy) *Clock {
	c := &Clock{strat: strat, tl: tl, tracks: make(map[uint32]*trackState)}
	for _, t := range tl.Tracks {
		st := &trackState{timescale: t.Timescale}
		st.stats.TrackID = t.ID
		for _, cl := range tl.Clips {
			if cl.TrackID == t.ID {
				st.segments = append(st.segments, segment{id: cl.ID, start: cl.StartTick, end: cl.StartTick + cl.DurationTick})
			}
		}
		for _, tr := range tl.Transitions {
			if tr.TrackID == t.ID {
				st.segments = append(st.segments, segment{id: tr.ID, start: tr.StartTick, end: tr.StartTick + tr.DurationTick, isTransition: true})
			}
		}
		c.tracks[t.ID] = st
	}
	return c
}

// wrapDomain 是 32 位时间戳域大小。
const wrapDomain = uint64(1) << 32

// Process 修复一个样本的时间戳并分配到片段。
func (c *Clock) Process(s model.Sample) (model.RepairedSample, error) {
	st, ok := c.tracks[s.TrackID]
	if !ok {
		return model.RepairedSample{}, fault.New(fault.StageRepair, s.TrackID, s.SourceOffset, false,
			fmt.Sprintf("未知轨道 %d", s.TrackID))
	}
	st.stats.Samples++
	rs := model.RepairedSample{
		Sample:      s,
		OriginalDTS: s.DTS,
		OriginalCTS: s.CTS,
	}

	expanded := s.DTS + st.wrapBase
	var flags model.RepairFlag

	if st.haveLast {
		switch {
		case s.DTS == st.lastRaw:
			flags |= model.RepairDuplicate
			st.stats.Duplicates++
			if c.strat.Duplicate == PolicyClamp {
				expanded = st.nextExpected
			}
		case s.DTS < st.lastRaw:
			if st.lastRaw-s.DTS > wrapDomain/2 {
				// 32 位时间戳回绕：展开到 64 位域。
				flags |= model.RepairWrap
				st.stats.Wraps++
				st.wrapBase += wrapDomain
				if c.strat.Wrap == PolicyClamp {
					expanded = s.DTS + st.wrapBase
				}
			} else {
				// 时间戳倒退。
				flags |= model.RepairRegression
				st.stats.Regressions++
				if c.strat.Regression == PolicyClamp {
					expanded = st.nextExpected
				}
			}
		default:
			// 跨剪辑跳变：间隔显著大于上一样本时长。
			if expanded > st.nextExpected && expanded-st.nextExpected > st.lastDur+st.lastDur/2 {
				flags |= model.RepairClipJump
				st.stats.ClipJumps++
				if c.strat.ClipJump == PolicyClamp {
					expanded = st.nextExpected
				}
			}
		}
	}

	// 漂移修正（按比例缩放，整数运算）。
	if c.strat.DriftPPM != 0 {
		ppm := c.strat.DriftPPM
		if ppm < 0 {
			expanded = expanded * uint64(1_000_000) / uint64(1_000_000-ppm)
		} else {
			expanded = expanded * uint64(1_000_000+ppm) / 1_000_000
		}
		flags |= model.RepairDrift
	}

	ctsOffset := int64(s.CTS) - int64(s.DTS)
	repairedCTS := uint64(int64(expanded) + ctsOffset)

	st.lastRaw = s.DTS
	st.haveLast = true
	st.lastDur = uint64(s.Duration)
	st.nextExpected = expanded + uint64(s.Duration)

	udts, err := model.ToUnified(st.timescale, expanded)
	if err != nil {
		return model.RepairedSample{}, fault.Wrap(fault.StageRepair, s.TrackID, s.SourceOffset, false, "时间基准溢出", err)
	}
	ucts, err := model.ToUnified(st.timescale, repairedCTS)
	if err != nil {
		return model.RepairedSample{}, fault.Wrap(fault.StageRepair, s.TrackID, s.SourceOffset, false, "时间基准溢出", err)
	}
	rs.UnifiedDTS = udts
	rs.UnifiedCTS = ucts
	rs.RepairFlags = flags
	rs.SegmentID, rs.IsTransition = st.segmentFor(expanded)
	return rs, nil
}

// segmentFor 查找 tick 所属片段；未命中时归入最近的前置片段。
func (st *trackState) segmentFor(tick uint64) (string, bool) {
	best := ""
	bestTrans := false
	var bestStart uint64
	for _, sg := range st.segments {
		if tick >= sg.start && tick < sg.end {
			return sg.id, sg.isTransition
		}
		if sg.start <= tick && (best == "" || sg.start > bestStart) {
			best, bestTrans, bestStart = sg.id, sg.isTransition, sg.start
		}
	}
	return best, bestTrans
}

// Stats 返回各轨道修复统计。
func (c *Clock) Stats() []TrackStats {
	out := make([]TrackStats, 0, len(c.tracks))
	for _, st := range c.tracks {
		out = append(out, st.stats)
	}
	return out
}
