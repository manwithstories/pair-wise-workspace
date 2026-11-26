package clock

import "sort"

// FrameKind 区分关键帧（可随机定位）与普通帧。
type FrameKind uint8

const (
	FrameInter FrameKind = iota
	FrameKey
)

// TimelineEvent 是统一 27 MHz 时间轴上的一个可索引媒体事件。
type TimelineEvent struct {
	TrackID uint16    // 轨道 PID
	Index   int       // 轨道内单调序号（由适配器解码给出）
	At      Tick      // 展开并校正后的统一时刻（保证全局非倒退）
	RawPTS  Tick      // 展开但未做单调性修正的原始 PTS，供审计
	Kind    FrameKind // 关键帧标记
	VFRGap  Tick      // 与本轨上一事件的原始间隔，VFR 间隔原样保留
}

// TrackTimeline 保存单条轨道的事件序列与可信状态。
type TrackTimeline struct {
	TrackID  uint16
	Events   []TimelineEvent
	Untrust  bool   // 连续性缺口后、下一关键帧之前为 true
	lastRaw  Tick   // 上一事件的展开 PTS
	haveLast bool
	lastOut  Tick
}

// Timeline 合并多条轨道，保证输出时刻单调不倒退，同时保留每轨 VFR 间隔。
type Timeline struct {
	tracks map[uint16]*TrackTimeline
}

// NewTimeline 创建空时间线。
func NewTimeline() *Timeline {
	return &Timeline{tracks: map[uint16]*TrackTimeline{}}
}

// Track 返回指定轨道，不存在时返回 nil。
func (t *Timeline) Track(id uint16) *TrackTimeline {
	return t.tracks[id]
}

// EnsureTrack 取得或创建轨道。
func (t *Timeline) EnsureTrack(id uint16) *TrackTimeline {
	tr, ok := t.tracks[id]
	if !ok {
		tr = &TrackTimeline{TrackID: id}
		t.tracks[id] = tr
	}
	return tr
}

// MarkUntrust 将轨道标记为不可信（连续性缺口传播）。
func (tr *TrackTimeline) MarkUntrust() { tr.Untrust = true }

// Append 追加一个事件。
//
// 不变量：输出 At 相对该轨上一事件严格非倒退；VFRGap 记录原始间隔，
// 渲染侧据此保留可变帧率节奏。轨道处于不可信状态时，只有关键帧能
// 解除标记（恢复点），普通帧仍被收录但保留 Untrust 痕迹。
func (tr *TrackTimeline) Append(ev TimelineEvent) TimelineEvent {
	if tr.haveLast {
		ev.VFRGap = ev.RawPTS - tr.lastRaw
		if ev.At <= tr.lastOut {
			// 时间戳不得倒退：至少推进 1 个 Tick。
			ev.At = tr.lastOut + 1
		}
	}
	tr.lastRaw = ev.RawPTS
	tr.lastOut = ev.At
	tr.haveLast = true
	if tr.Untrust && ev.Kind == FrameKey {
		tr.Untrust = false
	}
	ev.TrackID = tr.TrackID
	tr.Events = append(tr.Events, ev)
	return ev
}

// Keyframes 返回该轨道全部关键帧的事件下标（轨道内）。
func (tr *TrackTimeline) Keyframes() []int {
	var out []int
	for i, ev := range tr.Events {
		if ev.Kind == FrameKey {
			out = append(out, i)
		}
	}
	return out
}

// TrackIDs 按 PID 升序返回轨道列表，保证输出确定性。
func (t *Timeline) TrackIDs() []uint16 {
	ids := make([]uint16, 0, len(t.tracks))
	for id := range t.tracks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Flatten 返回按 (At, TrackID, Index) 排序的全轨事件副本。
func (t *Timeline) Flatten() []TimelineEvent {
	var all []TimelineEvent
	for _, id := range t.TrackIDs() {
		all = append(all, t.tracks[id].Events...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].At != all[j].At {
			return all[i].At < all[j].At
		}
		if all[i].TrackID != all[j].TrackID {
			return all[i].TrackID < all[j].TrackID
		}
		return all[i].Index < all[j].Index
	})
	return all
}

// Monotonic 校验全轨展开后的输出时刻是否严格非倒退（状态不变量自检）。
func (t *Timeline) Monotonic() bool {
	for _, id := range t.TrackIDs() {
		var prev Tick
		for i, ev := range t.tracks[id].Events {
			if i > 0 && ev.At < prev {
				return false
			}
			prev = ev.At
		}
	}
	return true
}
