// Package repair 识别并修复媒体时间戳的回绕、重复、倒退与跨剪辑跳变，
// 输出统一时间基准下的单调连续时间戳，原始值保留供审计。
package repair

import (
	"fmt"
	"sort"

	"mediarepair/internal/fault"
	"mediarepair/internal/model"
)

// Policy 是单类异常的修复策略。
type Policy string

const (
	PolicyClamp Policy = "clamp" // 钳制/重定基为单调连续
	PolicyKeep  Policy = "keep"  // 仅标记，不修改（审计模式）
)

// Strategy 汇总各类异常的修复策略；DriftPPM 非零时按比例修正时钟漂移。
type Strategy struct {
	Wrap       Policy
	Duplicate  Policy
	Regression Policy
	ClipJump   Policy
	DriftPPM   int64 // 正值表示源时钟偏快，按比例拉回
}

// DefaultStrategy 全部异常钳制修复，不做漂移修正。
func DefaultStrategy() Strategy {
	return Strategy{Wrap: PolicyClamp, Duplicate: PolicyClamp, Regression: PolicyClamp, ClipJump: PolicyClamp}
}

// AuditStrategy 只检测不修改，用于审计原始时间戳。
func AuditStrategy() Strategy {
	return Strategy{Wrap: PolicyKeep, Duplicate: PolicyKeep, Regression: PolicyKeep, ClipJump: PolicyKeep}
}

// TrackStats 是单轨修复统计。
type TrackStats struct {
	TrackID     uint32
	Samples     uint64
	Wraps       uint64
	Duplicates  uint64
	Regressions uint64
	ClipJumps   uint64
	Drifts      uint64
}

// SegmentInfo 描述一个剪辑或转场片段及其修复后的时间范围。
type SegmentInfo struct {
	ID           string
	TrackID      uint32
	IsTransition bool
	StartTick    uint64 // 轨道时钟逻辑起点
	EndTick      uint64
	Samples      uint64
	Repairs      uint64
}

// segment 是轨道上的一段连续区间（剪辑或转场）。
type segment struct {
	id           string
	start, end   uint64 // 轨道时钟逻辑区间
	isTransition bool
	first, count uint64 // 轨道内样本区间
}

type trackState struct {
	timescale uint32
	haveLast  bool
	lastRaw   uint64 // 上一次原始 DTS（未展开）
	wrapBase  uint64 // 回绕展开偏移
	nextExp   uint64 // 修复后轨道时钟期望的下一个 DTS
	lastDur   uint64
	nominal   uint64    // 观察到的最小正向样本时长，用于识别边界大跳变
	segments  []segment // 全部片段（按逻辑起点排序）
	ranged    []segment // 有样本区间的片段（按样本序号排序）
	rangeMode bool
	curSeg    *segment
	segBase   uint64 // 当前片段首个样本的展开后原始 DTS
	segSet    bool
	segStats  map[string]*SegmentInfo
	stats     TrackStats
}

// Clock 按轨道维护修复状态。
type Clock struct {
	strat  Strategy
	tl     *model.Timeline
	tracks map[uint32]*trackState
	order  []uint32
}

// NewClock 基于时间线构建修复器；时间线须先通过 Validate。
func NewClock(tl *model.Timeline, strat Strategy) *Clock {
	c := &Clock{strat: strat, tl: tl, tracks: make(map[uint32]*trackState)}
	for _, t := range tl.Tracks {
		st := &trackState{timescale: t.Timescale, segStats: make(map[string]*SegmentInfo)}
		st.stats.TrackID = t.ID
		for _, cl := range tl.Clips {
			if cl.TrackID == t.ID {
				st.addSegment(segment{id: cl.ID, start: cl.StartTick, end: cl.StartTick + cl.DurationTick,
					first: cl.FirstSample, count: cl.SampleCount})
			}
		}
		for _, tr := range tl.Transitions {
			if tr.TrackID == t.ID {
				st.addSegment(segment{id: tr.ID, start: tr.StartTick, end: tr.StartTick + tr.DurationTick,
					isTransition: true, first: tr.FirstSample, count: tr.SampleCount})
			}
		}
		sort.SliceStable(st.segments, func(i, j int) bool { return st.segments[i].start < st.segments[j].start })
		if len(st.ranged) > 0 {
			st.rangeMode = true
			sort.Slice(st.ranged, func(i, j int) bool { return st.ranged[i].first < st.ranged[j].first })
		}
		c.tracks[t.ID] = st
		c.order = append(c.order, t.ID)
	}
	return c
}

func (st *trackState) addSegment(sg segment) {
	st.segments = append(st.segments, sg)
	st.segStats[sg.id] = &SegmentInfo{ID: sg.id, TrackID: st.stats.TrackID, IsTransition: sg.isTransition,
		StartTick: sg.start, EndTick: sg.end}
	if sg.count > 0 {
		st.ranged = append(st.ranged, sg)
	}
}

// wrapDomain 是 32 位时间戳域大小。
const wrapDomain = uint64(1) << 32

// lookup 按样本序号（区间模式）或逻辑时间（兼容模式）定位片段。
func (st *trackState) lookup(seq, tick uint64) *segment {
	if st.rangeMode {
		i := sort.Search(len(st.ranged), func(i int) bool { return st.ranged[i].first > seq })
		if i == 0 {
			return nil
		}
		sg := &st.ranged[i-1]
		if seq >= sg.first && seq < sg.first+sg.count {
			return sg
		}
		return nil
	}
	best := -1
	for i := range st.segments {
		sg := &st.segments[i]
		if tick >= sg.start && tick < sg.end {
			return sg
		}
		if sg.start <= tick {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	return &st.segments[best]
}

// Process 修复一个样本的时间戳并分配到片段。
func (c *Clock) Process(s model.Sample) (model.RepairedSample, error) {
	st, ok := c.tracks[s.TrackID]
	if !ok {
		return model.RepairedSample{}, fault.New(fault.StageRepair, s.TrackID, s.SourceOffset, false,
			fmt.Sprintf("未知轨道 %d", s.TrackID))
	}
	st.stats.Samples++
	rs := model.RepairedSample{Sample: s, OriginalDTS: s.DTS, OriginalCTS: s.CTS}

	expanded := s.DTS + st.wrapBase
	var flags model.RepairFlag

	seg := st.lookup(s.Seq, expanded)
	newSeg := seg != nil && (st.curSeg == nil || st.curSeg.id != seg.id)

	// 先识别 32 位回绕：跨越 2^32 的负向跳变优先于片段边界判断。
	if st.haveLast && s.DTS < st.lastRaw && st.lastRaw-s.DTS > wrapDomain/2 {
		flags |= model.RepairWrap
		st.stats.Wraps++
		if c.strat.Wrap == PolicyClamp {
			st.wrapBase += wrapDomain
			expanded = s.DTS + st.wrapBase
		}
	}

	if newSeg {
		if st.haveLast && flags&model.RepairWrap == 0 && st.nominal > 0 {
			// 边界处原始间隔显著大于常规样本时长即为跨剪辑跳变。
			if s.DTS > st.lastRaw && uint64(s.DTS-st.lastRaw) > st.nominal+st.nominal/2 {
				flags |= model.RepairClipJump
				st.stats.ClipJumps++
			}
		}
		st.curSeg = seg
		st.segBase = expanded
		st.segSet = true
	} else if st.haveLast && flags&model.RepairWrap == 0 {
		switch {
		case s.DTS == st.lastRaw:
			flags |= model.RepairDuplicate
			st.stats.Duplicates++
		case s.DTS < st.lastRaw:
			flags |= model.RepairRegression
			st.stats.Regressions++
		}
	}

	repairedTick := st.mapTick(c.strat, seg, expanded, flags, newSeg)
	var err error
	if c.strat.DriftPPM != 0 {
		if repairedTick, err = applyDrift(repairedTick, c.strat.DriftPPM); err != nil {
			return model.RepairedSample{}, fault.Wrap(fault.StageRepair, s.TrackID, s.SourceOffset, false, "漂移修正溢出", err)
		}
		flags |= model.RepairDrift
		st.stats.Drifts++
	}

	// CTS 保留相对 DTS 的偏移，随修复后时间域一起平移。
	ctsOffset := int64(s.CTS+st.wrapBase) - int64(expanded)
	repairedCTSU := uint64(int64(repairedTick) + ctsOffset)
	if c.strat.DriftPPM != 0 {
		if repairedCTSU, err = applyDrift(repairedCTSU, c.strat.DriftPPM); err != nil {
			return model.RepairedSample{}, fault.Wrap(fault.StageRepair, s.TrackID, s.SourceOffset, false, "漂移修正溢出", err)
		}
	}

	udts, err := model.ToUnified(st.timescale, repairedTick)
	if err != nil {
		return model.RepairedSample{}, fault.Wrap(fault.StageRepair, s.TrackID, s.SourceOffset, false, "时间基准溢出", err)
	}
	ucts, err := model.ToUnified(st.timescale, repairedCTSU)
	if err != nil {
		return model.RepairedSample{}, fault.Wrap(fault.StageRepair, s.TrackID, s.SourceOffset, false, "时间基准溢出", err)
	}

	st.lastRaw = s.DTS
	st.haveLast = true
	st.lastDur = uint64(s.Duration)
	if !newSeg && s.Duration > 0 && (st.nominal == 0 || uint64(s.Duration) < st.nominal) {
		st.nominal = uint64(s.Duration)
	}
	st.nextExp = repairedTick + uint64(s.Duration)

	rs.RepairedDTS = repairedTick
	rs.RepairedCTS = repairedCTSU
	rs.UnifiedDTS = udts
	rs.UnifiedCTS = ucts
	rs.RepairFlags = flags
	if seg != nil {
		rs.SegmentID = seg.id
		rs.IsTransition = seg.isTransition
		if si := st.segStats[seg.id]; si != nil {
			si.Samples++
			if flags != model.RepairNone {
				si.Repairs++
			}
		}
	}
	return rs, nil
}

// mapTick 把展开后的原始 tick 映射到片段逻辑时间，并按要求钳制。
// 钳制时保证同一片段内 DTS 严格递增；片段首样本允许重定基。
func (st *trackState) mapTick(strat Strategy, seg *segment, expanded uint64, flags model.RepairFlag, newSeg bool) uint64 {
	clamp := strat.Wrap == PolicyClamp && strat.Duplicate == PolicyClamp && strat.Regression == PolicyClamp
	var tick uint64
	if seg != nil && st.segSet {
		if expanded >= st.segBase {
			tick = seg.start + (expanded - st.segBase)
		} else {
			tick = seg.start
		}
	} else {
		tick = expanded
	}
	if !clamp || newSeg {
		return tick
	}
	if flags&(model.RepairDuplicate|model.RepairRegression) != 0 || tick < st.nextExp {
		if tick < st.nextExp {
			return st.nextExp
		}
	}
	return tick
}

// applyDrift 按 PPM 比例缩放时间戳；负值表示源时钟偏慢需要拉长。
func applyDrift(tick uint64, ppm int64) (uint64, error) {
	const scale = uint64(1_000_000)
	if ppm > 0 {
		d := uint64(ppm)
		if tick > (^uint64(0)-d)/scale {
			return 0, model.ErrOverflow
		}
		return tick * (scale + d) / scale, nil
	}
	d := uint64(-ppm)
	if d >= scale {
		return 0, fmt.Errorf("%w: 非法漂移 %d ppm", model.ErrOverflow, ppm)
	}
	if tick > (^uint64(0))/scale {
		return 0, model.ErrOverflow
	}
	return tick * scale / (scale - d), nil
}

// Stats 返回各轨道修复统计，按轨道 ID 排序以保证输出确定。
func (c *Clock) Stats() []TrackStats {
	out := make([]TrackStats, 0, len(c.tracks))
	for _, id := range c.sortIDs() {
		out = append(out, c.tracks[id].stats)
	}
	return out
}

func (c *Clock) sortIDs() []uint32 {
	ids := make([]uint32, 0, len(c.tracks))
	for id := range c.tracks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Segments 返回全部片段的修复统计，按轨道 ID 与起点排序。
func (c *Clock) Segments() []SegmentInfo {
	var out []SegmentInfo
	for _, id := range c.sortIDs() {
		st := c.tracks[id]
		ids := make([]string, 0, len(st.segStats))
		for sid := range st.segStats {
			ids = append(ids, sid)
		}
		sort.Slice(ids, func(i, j int) bool { return st.segStats[ids[i]].StartTick < st.segStats[ids[j]].StartTick })
		for _, sid := range ids {
			out = append(out, *st.segStats[sid])
		}
	}
	return out
}
