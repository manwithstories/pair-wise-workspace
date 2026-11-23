// Package clock 定义统一的纳秒媒体时钟、
// 32 位时间戳回绕扩展与统一错误模型。
package clock

import "fmt"

// Nanos 是统一媒体时钟的单位（纳秒）。
type Nanos int64

// 常用时间单位。
const (
	Microsecond Nanos = 1000
	Millisecond Nanos = 1000 * Microsecond
	Second      Nanos = 1000 * Millisecond
)

// TicksToNanos 按 num/den 的时间基把刻度转换为纳秒。
// 先除后乘，避免大刻度值溢出。
func TicksToNanos(ticks, num, den int64) Nanos {
	if den == 0 {
		return 0
	}
	q, r := ticks/den, ticks%den
	return Nanos(q*num + r*num/den)
}

// Unwrapper 把 32 位环绕时间戳扩展为单调的 64 位值。
// 回绕按双向最近距离解释，允许乱序到达。
type Unwrapper struct {
	init bool
	ext  int64
}

// Extend 输入最新 32 位时间戳，返回扩展后的 64 位值。
func (u *Unwrapper) Extend(ts uint32) int64 {
	if !u.init {
		u.init = true
		u.ext = int64(ts)
		return u.ext
	}
	delta := int32(ts - uint32(u.ext))
	u.ext += int64(delta)
	return u.ext
}

// ErrKind 是统一错误模型的分类。
type ErrKind int

// 时间戳违规分类。
const (
	ErrNegativeTimestamp ErrKind = iota // 负时间戳
	ErrNonMonotonic                     // 同轨道非单调
)

// StampError 描述一次时间戳违规；输入绝不被静默改写。
type StampError struct {
	Kind  ErrKind
	Track uint64
	Value Nanos
	Prev  Nanos
}

func (e *StampError) Error() string {
	kind := "unknown"
	switch e.Kind {
	case ErrNegativeTimestamp:
		kind = "negative timestamp"
	case ErrNonMonotonic:
		kind = "non-monotonic timestamp"
	}
	return fmt.Sprintf("clock: track %d: %s: value=%d prev=%d",
		e.Track, kind, int64(e.Value), int64(e.Prev))
}

// Timeline 按轨道校验时间戳序列。
type Timeline struct {
	last map[uint64]Nanos
	seen map[uint64]bool
}

// NewTimeline 创建空时间线。
func NewTimeline() *Timeline {
	return &Timeline{last: make(map[uint64]Nanos), seen: make(map[uint64]bool)}
}

// Validate 校验一个时间戳。违规时返回 *StampError，
// 且不更新内部状态（负值与非单调值不进入基准）。
func (t *Timeline) Validate(track uint64, ts Nanos) error {
	if ts < 0 {
		return &StampError{Kind: ErrNegativeTimestamp, Track: track, Value: ts, Prev: t.last[track]}
	}
	if t.seen[track] && ts < t.last[track] {
		return &StampError{Kind: ErrNonMonotonic, Track: track, Value: ts, Prev: t.last[track]}
	}
	t.last[track] = ts
	t.seen[track] = true
	return nil
}
