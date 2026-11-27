// Package timeline 实现每轨水位推进、迟到帧确定性重算与超窗隔离。
package timeline

import (
	"sort"

	"relay/internal/media"
)

// 隔离原因。
const (
	ReasonLateBeyondWindow  = "late_beyond_window" // 事件时间早于水位，超出可回溯窗口
	ReasonDuplicateConflict = "duplicate_conflict" // 同序号哈希冲突（由管线登记）
)

// Envelope 是进入时间线的帧及其解析后的轨道。
type Envelope struct {
	TrackID uint32
	Frame   *media.Frame
}

// Key 返回确定性排序键：事件时间、轨道、序号。
func (e *Envelope) Key() [3]uint64 {
	return [3]uint64{uint64(e.Frame.EventNS), uint64(e.TrackID), e.Frame.Sequence}
}

// LessKey 按 (事件时间, 轨道, 序号) 字典序比较两个排序键。
func LessKey(a, b [3]uint64) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// Quarantine 记录被隔离的帧及原因；不进入正常输出。
type Quarantine struct {
	Env    *Envelope
	Reason string
	Detail string
}

// State 是可随检查点持久化的时间线状态。
type State struct {
	MaxEvent map[uint32]int64 // 每轨已见最大事件时间（纳秒）
	GraceNS  int64
}

// Engine 是有界回溯时间线。水位 = 每轨最大事件时间 - 250ms；
// 事件时间不早于水位的迟到帧留在回溯区内按确定性键重排，
// 早于水位的帧直接隔离。检查点只在回溯区排空时建立，因此
// 恢复后重放输入即可按 (时间,轨道,序号) 确定性重算。
type Engine struct {
	maxEvent map[uint32]media.TimeNS
	pending  map[uint32][]*Envelope // 未跨越水位的帧，每轨按键有序
	grace    media.TimeNS
	quar     []*Quarantine
}

// NewEngine 创建引擎，grace 通常取 media.GraceWindow。
func NewEngine(grace media.TimeNS) *Engine {
	return &Engine{
		maxEvent: make(map[uint32]media.TimeNS),
		pending:  make(map[uint32][]*Envelope),
		grace:    grace,
	}
}

// Watermark 返回某轨当前水位；未见帧的轨道返回 0。
func (e *Engine) Watermark(track uint32) media.TimeNS {
	mx, ok := e.maxEvent[track]
	if !ok {
		return 0
	}
	return mx - e.grace
}

// Ingest 接收一帧，返回本次因此跨越水位而确定化释放的帧序列。
// 超窗迟到帧进入隔离列表而非释放。
func (e *Engine) Ingest(env *Envelope) []*Envelope {
	track := env.TrackID
	if mx, ok := e.maxEvent[track]; !ok || env.Frame.EventNS > mx {
		// 首次见该轨道时，水位不应把历史帧判为迟到：首帧只建基准。
		if !ok {
			e.maxEvent[track] = env.Frame.EventNS
			e.insertPending(track, env)
			return nil
		}
		e.maxEvent[track] = env.Frame.EventNS
	}
	// 迟到判定：事件时间早于当前水位即超出 250ms 回溯窗口。
	if env.Frame.EventNS < e.Watermark(track) {
		e.quar = append(e.quar, &Quarantine{
			Env:    env,
			Reason: ReasonLateBeyondWindow,
			Detail: "event_ns below track watermark",
		})
		return nil
	}
	e.insertPending(track, env)
	return e.drain(track)
}

// insertPending 按键有序插入回溯区（迟到帧的确定性重算）。
func (e *Engine) insertPending(track uint32, env *Envelope) {
	q := e.pending[track]
	k := env.Key()
	pos := sort.Search(len(q), func(i int) bool { return LessKey(k, q[i].Key()) })
	q = append(q, nil)
	copy(q[pos+1:], q[pos:])
	q[pos] = env
	e.pending[track] = q
}

// drain 输出已跨越水位的帧；同一批次内严格按键有序。
func (e *Engine) drain(track uint32) []*Envelope {
	wm := e.Watermark(track)
	q := e.pending[track]
	cut := 0
	for cut < len(q) && q[cut].Frame.EventNS <= wm {
		cut++
	}
	if cut == 0 {
		return nil
	}
	out := make([]*Envelope, cut)
	copy(out, q[:cut])
	e.pending[track] = q[cut:]
	return out
}

// QuarantineExplicit 供管线登记非水位原因的隔离（如序号哈希冲突）。
func (e *Engine) QuarantineExplicit(env *Envelope, reason, detail string) {
	e.quar = append(e.quar, &Quarantine{Env: env, Reason: reason, Detail: detail})
}

// Quarantine 返回累积的隔离记录。
func (e *Engine) Quarantine() []*Quarantine { return e.quar }

// Flush 在输入结束时排空全部回溯区，返回全局确定性排序的帧。
func (e *Engine) Flush() []*Envelope {
	var all []*Envelope
	for _, q := range e.pending {
		all = append(all, q...)
	}
	e.pending = make(map[uint32][]*Envelope)
	sort.Slice(all, func(i, j int) bool { return LessKey(all[i].Key(), all[j].Key()) })
	return all
}

// PendingCount 返回回溯区在途帧数（检查点安全点判定用）。
func (e *Engine) PendingCount() int {
	n := 0
	for _, q := range e.pending {
		n += len(q)
	}
	return n
}

// ExportState 导出可持久化状态（仅在回溯区排空时调用）。
func (e *Engine) ExportState() State {
	s := State{MaxEvent: make(map[uint32]int64, len(e.maxEvent)), GraceNS: int64(e.grace)}
	for t, mx := range e.maxEvent {
		s.MaxEvent[t] = int64(mx)
	}
	return s
}

// RestoreState 从检查点重建引擎。
func (e *Engine) RestoreState(s State) {
	e.grace = media.TimeNS(s.GraceNS)
	e.maxEvent = make(map[uint32]media.TimeNS, len(s.MaxEvent))
	for t, mx := range s.MaxEvent {
		e.maxEvent[t] = media.TimeNS(mx)
	}
	e.pending = make(map[uint32][]*Envelope)
}
