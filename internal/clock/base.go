// Package clock 定义各轨共用的有理数时间基准，
// 负责把不同 timescale 的时间戳映射到统一基准，
// 并处理时间戳回退、回绕及断点重置。
package clock

import (
	"errors"
	"fmt"
)

// Rational 以有理数表示的秒数。
type Rational struct {
	Num int64
	Den int64
}

// Norm 约分并保证分母为正。
func (r Rational) Norm() Rational {
	if r.Den < 0 {
		r.Num, r.Den = -r.Num, -r.Den
	}
	if r.Num == 0 {
		return Rational{0, 1}
	}
	g := GCD(abs64(r.Num), r.Den)
	return Rational{r.Num / g, r.Den / g}
}

// ToMillis 转换为毫秒浮点。
func (r Rational) ToMillis() float64 {
	return float64(r.Num) * 1000.0 / float64(r.Den)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// GCD 最大公约数。
func GCD(a, b int64) int64 {
	a, b = abs64(a), abs64(b)
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// LCM 最小公倍数，溢出时返回 0。
func LCM(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	g := GCD(a, b)
	q := a / g
	if b != 0 && abs64(b) > (1<<62)/abs64(q) {
		return 0
	}
	return abs64(q * b)
}

// Base 统一时间基准：1/Timescale 秒为一个 tick。
type Base struct {
	Timescale int64
}

// NewBase 以所有轨道 timescale 的最小公倍数构建统一基准。
func NewBase(timescales ...int64) (Base, error) {
	if len(timescales) == 0 {
		return Base{}, errors.New("clock: no timescale given")
	}
	l := timescales[0]
	if l <= 0 {
		return Base{}, fmt.Errorf("clock: invalid timescale %d", l)
	}
	for _, ts := range timescales[1:] {
		if ts <= 0 {
			return Base{}, fmt.Errorf("clock: invalid timescale %d", ts)
		}
		l = LCM(l, ts)
		if l == 0 {
			return Base{}, errors.New("clock: timescale LCM overflow")
		}
	}
	return Base{Timescale: l}, nil
}

// Map 把 timescale 下的时间戳 ts 映射为基准 tick，要求整除。
func (b Base) Map(ts, timescale int64) (int64, error) {
	if timescale <= 0 {
		return 0, fmt.Errorf("clock: invalid timescale %d", timescale)
	}
	if b.Timescale%timescale != 0 {
		return 0, fmt.Errorf("clock: base %d not divisible by timescale %d", b.Timescale, timescale)
	}
	return ts * (b.Timescale / timescale), nil
}

// Millis 把基准 tick 转换为毫秒。
func (b Base) Millis(ticks int64) float64 {
	return Rational{ticks, b.Timescale}.ToMillis()
}

// Tracker 跟踪单轨时间戳流，处理回绕与断点重置。
type Tracker struct {
	base      Base
	timescale int64
	wrapMod   int64 // 时间戳回绕模数，如 1<<32
	last      int64
	wraps     int64
	offset    int64 // 断点重置后的基准 tick 偏移
	started   bool
}

// NewTracker 创建轨道时间戳跟踪器。
func NewTracker(base Base, timescale, wrapMod int64) *Tracker {
	return &Tracker{base: base, timescale: timescale, wrapMod: wrapMod}
}

// Reset 在 discontinuity 处重置：之后的时间戳从 offset 基准 tick 起算。
func (t *Tracker) Reset(offsetTicks int64) {
	t.offset = offsetTicks
	t.started = false
	t.wraps = 0
	t.last = 0
}

// ErrRollback 表示时间戳回退（非回绕）。
type ErrRollback struct {
	Last, Cur int64
}

func (e *ErrRollback) Error() string {
	return fmt.Sprintf("clock: timestamp rollback %d -> %d", e.Last, e.Cur)
}

// Map 映射时间戳；检测回绕（ts 大幅小于 last）与回退（小幅倒退，报错）。
func (t *Tracker) Map(ts int64) (int64, error) {
	if t.started && ts < t.last {
		if t.wrapMod > 0 && t.last-ts > t.wrapMod/2 {
			t.wraps++
		} else {
			return 0, &ErrRollback{Last: t.last, Cur: ts}
		}
	}
	t.started = true
	t.last = ts
	tick, err := t.base.Map(ts+t.wraps*t.wrapMod, t.timescale)
	if err != nil {
		return 0, err
	}
	return t.offset + tick, nil
}
