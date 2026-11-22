package timeline

import "fmt"

// 32 位 RTP 时间戳回绕空间。
const RTPWrapMod uint64 = 1 << 32

// MappedPoint 是一个成功映射的共同时间线点。
type MappedPoint struct {
	TrackID uint32
	RawTick int64  // 原始 tick（回绕前值原样保留）
	Ns      int64  // 共同时间线纳秒
	ErrNs   int64  // 时钟量化误差（纳秒）
	Wrap    uint64 // 累计跨越的回绕周期数
}

// Mapper 按轨道维护时钟偏移与时间戳回绕计数。
// 映射失败只返回 *MapError，绝不猜测时间戳。
type Mapper struct {
	graph *Graph
	last  map[uint32]uint64 // 已展开的最近 tick（无符号空间）
	wrap  map[uint32]uint64
}

func NewMapper(g *Graph) *Mapper {
	return &Mapper{
		graph: g,
		last:  make(map[uint32]uint64),
		wrap:  make(map[uint32]uint64),
	}
}

// Map 将某轨道的一个 tick 映射到共同时间线。
// 首次出现的 tick 不做回绕假设；后续 tick 若显著回退则按一个回绕周期展开，
// 展开量受 maxBackStep 限制，异常回退直接报错并保留原始值。
func (m *Mapper) Map(trackID uint32, tick uint64, maxBackStep uint64) (MappedPoint, error) {
	t, ok := m.graph.Get(trackID)
	if !ok {
		return MappedPoint{}, &MapError{TrackID: trackID, RawTick: tick,
			Kind: "unknown", Reason: "track not in timeline graph"}
	}
	if t.Clock == nil {
		return MappedPoint{}, &MapError{TrackID: trackID, Kind: t.Kind,
			RawTick: tick, Reason: "track has no clock"}
	}
	expanded := tick
	wrapCount := m.wrap[trackID]
	if prev, seen := m.last[trackID]; seen {
		if tick < prev && maxBackStep > 0 {
			gap := prev - tick
			if gap > maxBackStep {
				expanded = tick + RTPWrapMod
				wrapCount++
			} else {
				return MappedPoint{}, &MapError{TrackID: trackID, Kind: t.Kind,
					RawTick: tick, Reason: fmt.Sprintf("non-monotonic tick, back-step %d", gap)}
			}
		}
	}
	ns, errNs, err := t.Clock.ToNanos(int64(expanded))
	if err != nil {
		return MappedPoint{}, err
	}
	m.last[trackID] = expanded
	m.wrap[trackID] = wrapCount
	return MappedPoint{
		TrackID: trackID, RawTick: int64(tick), Ns: ns,
		ErrNs: errNs, Wrap: wrapCount,
	}, nil
}

// Wraps 返回某轨道累计回绕次数。
func (m *Mapper) Wraps(trackID uint32) uint64 { return m.wrap[trackID] }

// Offset 调整某轨道时钟的 epoch（纳秒），用于多机位对齐。
func (m *Mapper) Offset(trackID uint32, epochNs int64) error {
	t, ok := m.graph.Get(trackID)
	if !ok {
		return &MapError{TrackID: trackID, Reason: "track not in timeline graph"}
	}
	if t.Clock == nil {
		return &MapError{TrackID: trackID, Kind: t.Kind, Reason: "track has no clock"}
	}
	t.Clock.EpochNs = epochNs
	return nil
}

// Reset 清空回绕状态（重跑前调用，保证两次运行从相同状态出发）。
func (m *Mapper) Reset() {
	for k := range m.last {
		delete(m.last, k)
	}
	for k := range m.wrap {
		delete(m.wrap, k)
	}
}
