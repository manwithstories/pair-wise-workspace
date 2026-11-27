package packet

import (
	"bytes"
	"errors"

	"archivemedia/internal/media"
)

// Decision 是去重判定结果。
type Decision int

const (
	// DecisionAccepted 新键，事件已入列。
	DecisionAccepted Decision = iota + 1
	// DecisionDuplicate 同键同内容，事件被丢弃（不计入恢复结果）。
	DecisionDuplicate
	// DecisionReplaced 同键异内容冲突，新事件按确定性规则胜出并替换旧事件。
	DecisionReplaced
	// DecisionKeptExisting 同键异内容冲突，既有事件按确定性规则保留。
	DecisionKeptExisting
)

// ConflictError 记录同键异内容冲突，供清单与终端输出统计。
// 它不是致命错误：流水线依据确定性选择规则继续。
type ConflictError struct {
	Key      Key
	Chosen   *Event
	Rejected *Event
}

// Error 实现 error。
func (c *ConflictError) Error() string {
	return media.E(media.KindPacketCorrupt, "packet.dedup",
		c.Chosen.Location(), "同键异内容冲突，已按确定性规则选择", nil).Error()
}

// Unwrap 让冲突可被 media.IsKind 识别为包级问题。
func (c *ConflictError) Unwrap() error {
	return media.E(media.KindPacketCorrupt, "packet.dedup",
		c.Chosen.Location(), "同键异内容冲突", nil)
}

// Deduper 按 (流, 纪元, 序号) 去重。
//
// 选择规则完全由键与载荷决定，与到达顺序无关：
//  1. 同键同内容（CRC 相同）→ 保留先到者，后者记为重复；
//  2. 同键异内容 → CRC 较小者胜出；CRC 相同则逐字节字典序较小者胜出；
//  3. 仍完全相同即退化为情况 1。
type Deduper struct {
	events    map[Key]*Event
	conflicts []ConflictError
	dupes     int
}

// NewDeduper 构造空去重器。
func NewDeduper() *Deduper {
	return &Deduper{events: make(map[Key]*Event)}
}

// Observe 处理一条事件。返回判定结果；冲突时同时返回 *ConflictError。
// 事件必须先通过 Validate；占位事件与普通事件同样参与去重。
func (d *Deduper) Observe(e *Event) (Decision, error) {
	if err := e.Validate(); err != nil {
		return 0, err
	}
	k := e.Key()
	old, ok := d.events[k]
	if !ok {
		d.events[k] = cloneEvent(e)
		return DecisionAccepted, nil
	}
	// 内容相等以载荷字节为唯一判据；同键下编码/时钟率等元数据由流定义保证一致。
	if bytes.Equal(old.Payload, e.Payload) {
		d.dupes++
		return DecisionDuplicate, nil
	}
	winner, loser := chooseWinner(old, e)
	conflict := ConflictError{Key: k, Chosen: winner, Rejected: loser}
	d.conflicts = append(d.conflicts, conflict)
	// winner 始终是内部存储的独立副本，绝不暴露调用方入参指针。
	if winner == old {
		return DecisionKeptExisting, &conflict
	}
	d.events[k] = cloneEvent(e)
	return DecisionReplaced, &conflict
}

// chooseWinner 是与到达顺序无关的纯函数选择规则。
// 注意：仅以指针身份区分 old/new，返回值不得逃逸到调用方；
// ConflictError 中的指针指向去重器内部副本（old）或入参的只读快照。
func chooseWinner(old, incoming *Event) (winner, loser *Event) {
	less := func(x, y *Event) bool {
		if x.CRC != y.CRC && (x.CRC != 0 || y.CRC != 0) {
			return x.CRC < y.CRC
		}
		return bytes.Compare(x.Payload, y.Payload) < 0
	}
	if less(old, incoming) {
		return old, cloneEvent(incoming)
	}
	if less(incoming, old) {
		return cloneEvent(incoming), old
	}
	// 完全等价（理论上前面已被同内容分支拦截），稳定保留既有事件。
	return old, cloneEvent(incoming)
}

// Get 返回键对应事件与是否存在。
func (d *Deduper) Get(k Key) (*Event, bool) {
	e, ok := d.events[k]
	return e, ok
}

// Len 返回当前唯一事件数。
func (d *Deduper) Len() int { return len(d.events) }

// Duplicates 返回累计重复（同键同内容）次数。
func (d *Deduper) Duplicates() int { return d.dupes }

// Conflicts 返回累计同键异内容冲突快照。
func (d *Deduper) Conflicts() []ConflictError {
	out := make([]ConflictError, len(d.conflicts))
	copy(out, d.conflicts)
	return out
}

// Snapshot 返回全部唯一事件，按键确定性排序（流→纪元→展开前序号）。
func (d *Deduper) Snapshot() []*Event {
	keys := make([]Key, 0, len(d.events))
	for k := range d.events {
		keys = append(keys, k)
	}
	// 简单插入排序：恢复场景单流规模可控，且避免额外依赖。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keyLess(keys[j], keys[j-1]); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	out := make([]*Event, 0, len(keys))
	for _, k := range keys {
		out = append(out, d.events[k])
	}
	return out
}

func keyLess(a, b Key) bool {
	if a.StreamID != b.StreamID {
		return a.StreamID < b.StreamID
	}
	if a.Epoch != b.Epoch {
		return a.Epoch < b.Epoch
	}
	return a.Seq < b.Seq
}

func cloneEvent(e *Event) *Event {
	cp := *e
	cp.Payload = append([]byte(nil), e.Payload...)
	return &cp
}

// IsConflict 判断错误链中是否包含冲突。
func IsConflict(err error) bool {
	var c *ConflictError
	return errors.As(err, &c)
}
