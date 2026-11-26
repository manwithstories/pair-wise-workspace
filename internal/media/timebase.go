// Package media 提供共享时基、时钟漂移校正与时间线合成能力。
package media

import (
	"fmt"
	"math"
)

// Rational 表示一个有理数，Den 恒为正且已约分。
type Rational struct {
	Num int64
	Den int64
}

// NewRational 构造并约分一个有理数。
func NewRational(num, den int64) Rational {
	return Rational{Num: num, Den: den}.Norm()
}

// Norm 约分并保证分母为正。
func (r Rational) Norm() Rational {
	if r.Den < 0 {
		r.Num, r.Den = -r.Num, -r.Den
	}
	if r.Num == 0 {
		r.Den = 1
		return r
	}
	g := gcd(abs64(r.Num), r.Den)
	if g > 1 {
		r.Num /= g
		r.Den /= g
	}
	return r
}

// Float64 返回浮点近似值。
func (r Rational) Float64() float64 {
	return float64(r.Num) / float64(r.Den)
}

// Timebase 时基：1 个 PTS 刻度对应的秒数 = Num/Den。
type Timebase struct {
	Num int64
	Den int64
}

// Validate 校验时基合法（正数、非零）。
func (tb Timebase) Validate() error {
	if tb.Num <= 0 || tb.Den <= 0 {
		return fmt.Errorf("media: invalid timebase %d/%d", tb.Num, tb.Den)
	}
	return nil
}

// ToRational 将 PTS 刻度换算为有理数秒。
func (tb Timebase) ToRational(pts int64) Rational {
	return NewRational(pts*tb.Num, tb.Den)
}

// ToMillis 将 PTS 刻度换算为毫秒。
func (tb Timebase) ToMillis(pts int64) float64 {
	return float64(pts) * float64(tb.Num) * 1000 / float64(tb.Den)
}

// FromMillis 将毫秒换算为最接近的刻度（四舍五入）。
func (tb Timebase) FromMillis(ms float64) int64 {
	return int64(math.Round(ms * float64(tb.Den) / (float64(tb.Num) * 1000)))
}

// DurationMillis 将刻度时长换算为毫秒。
func (tb Timebase) DurationMillis(ticks int64) float64 {
	return tb.ToMillis(ticks)
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
