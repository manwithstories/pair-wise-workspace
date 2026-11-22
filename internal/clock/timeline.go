// Package clock 定义统一纳秒时间基准，所有子系统共享。
package clock

import "fmt"

// Ticks 是统一时间基准，单位为纳秒。
type Ticks int64

// Second 每秒的纳秒数。
const Second Ticks = 1_000_000_000

// SamplesToTicks 把采样序号（基于采样时间而非到达时间）换算为纳秒。
// rate 必须为正，否则 panic（调用方负责校验并返回可定位错误）。
func SamplesToTicks(samples int64, rate int) Ticks {
	if rate <= 0 {
		panic(fmt.Sprintf("clock: invalid sample rate %d", rate))
	}
	return Ticks(samples * int64(Second) / int64(rate))
}

// TicksToSamples 把纳秒换算回采样序号（向下取整）。
func TicksToSamples(t Ticks, rate int) int64 {
	if rate <= 0 {
		panic(fmt.Sprintf("clock: invalid sample rate %d", rate))
	}
	return int64(t) * int64(rate) / int64(Second)
}

// RTPTimestampToTicks 把 RTP 时间戳（时钟频率 clockRate）换算为纳秒。
func RTPTimestampToTicks(ts uint32, clockRate int) Ticks {
	return Ticks(int64(ts) * int64(Second) / int64(clockRate))
}

// Mapping 描述一段输入时间轴到统一时钟的显式映射：
// 输入时间轴上 LocalBase 对应统一时钟上的 UnifiedBase。
// 用于 HLS discontinuity 等显式时钟重置场景。
type Mapping struct {
	LocalBase   Ticks // 输入本地时间轴基准
	UnifiedBase Ticks // 统一时钟基准
}

// Map 把本地时间映射到统一时钟。
func (m Mapping) Map(local Ticks) Ticks { return local - m.LocalBase + m.UnifiedBase }

// Timeline 由一串不重叠的映射段组成，把（可能含时钟重置的）
// 输入时间轴无歧义地映射到统一纳秒时钟。
type Timeline struct {
	segs []Mapping
}

// NewTimeline 以原点映射创建时间线。
func NewTimeline() *Timeline {
	return &Timeline{segs: []Mapping{{LocalBase: 0, UnifiedBase: 0}}}
}

// Reset 在本地时间 local 处显式重置时钟：local 起映射到 unified。
// 若 local 早于上一段基准（时间回退且无显式映射）返回错误。
func (t *Timeline) Reset(local, unified Ticks) error {
	last := t.segs[len(t.segs)-1]
	if local < last.LocalBase {
		return fmt.Errorf("clock: ambiguous reset at local=%d before previous base=%d", local, last.LocalBase)
	}
	t.segs = append(t.segs, Mapping{LocalBase: local, UnifiedBase: unified})
	return nil
}

// Map 把本地时间映射到统一时钟；local 早于首段基准时返回错误。
func (t *Timeline) Map(local Ticks) (Ticks, error) {
	if local < 0 {
		return 0, fmt.Errorf("clock: negative local time %d cannot be mapped", local)
	}
	m := t.segs[0]
	for _, s := range t.segs {
		if local >= s.LocalBase {
			m = s
		}
	}
	return m.Map(local), nil
}

// Segments 返回映射段副本（供报告与调试）。
func (t *Timeline) Segments() []Mapping {
	out := make([]Mapping, len(t.segs))
	copy(out, t.segs)
	return out
}
