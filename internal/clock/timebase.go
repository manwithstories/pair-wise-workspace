// Package clock 提供统一的整数有理数时间基准与跨轨换算。
//
// 所有时间换算都在整数域完成，拒绝零分母与溢出，四舍五入保证跨轨
// 对齐误差不超过目标时基的一个最小单位（即不超过一个最小样本时长）。
package clock

import (
	"errors"
	"math"
)

// 有理数换算相关错误。
var (
	// ErrZeroDenominator 表示构造有理数或换算时出现零分母。
	ErrZeroDenominator = errors.New("clock: zero denominator")
	// ErrOverflow 表示中间结果超出 int64 可表示范围。
	ErrOverflow = errors.New("clock: arithmetic overflow")
)

// Rational 表示 Num/Den 的有理数，Den 恒为正。
type Rational struct {
	Num int64
	Den int64
}

// New 构造约分后的有理数，拒绝零分母并统一分母符号。
func New(num, den int64) (Rational, error) {
	if den == 0 {
		return Rational{}, ErrZeroDenominator
	}
	if den < 0 {
		if num == math.MinInt64 || den == math.MinInt64 {
			return Rational{}, ErrOverflow
		}
		num, den = -num, -den
	}
	if num == 0 {
		return Rational{Num: 0, Den: 1}, nil
	}
	g := gcd(abs64(num), den)
	return Rational{Num: num / g, Den: den / g}, nil
}

// MustNew 与 New 相同，但在非法输入时 panic，仅供常量式内部使用。
func MustNew(num, den int64) Rational {
	r, err := New(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

// Equal 判断两个有理数是否相等。
func (r Rational) Equal(o Rational) bool {
	return r.Num == o.Num && r.Den == o.Den
}

// Invert 返回倒数，零值有理数取倒数会返回零分母错误。
func (r Rational) Invert() (Rational, error) {
	return New(r.Den, r.Num)
}

// Mul 返回 r 与 o 的乘积，中间结果溢出时返回错误。
func (r Rational) Mul(o Rational) (Rational, error) {
	return New(mulCheck(r.Num, o.Num), mulCheck(r.Den, o.Den))
}

// Scale 计算 v * Num / Den，结果按“半数远离零”规则取整。
func (r Rational) Scale(v int64) (int64, error) {
	if r.Den <= 0 {
		return 0, ErrZeroDenominator
	}
	if r.Num == 0 || v == 0 {
		return 0, nil
	}
	p, err := mulCheck2(v, r.Num)
	if err != nil {
		return 0, err
	}
	q := p / r.Den
	rem := p % r.Den
	// rem 与 p 同号；当余数绝对值 >= 分母一半时远离零取整。
	if abs64(rem)*2 >= r.Den {
		if p > 0 {
			q++
		} else {
			q--
		}
	}
	return q, nil
}

// Convert 把数值 v 从源时基 from 换算到目标时基 to。
// from/to 的语义均为“每秒钟包含的 tick 数”（Num tick / Den 秒）。
func Convert(v int64, from, to Rational) (int64, error) {
	if from.Num == 0 || to.Num == 0 {
		return 0, ErrZeroDenominator
	}
	// 目标 tick = v * from.Den * to.Num / (from.Num * to.Den)
	num, err := mulCheck2(from.Den, to.Num)
	if err != nil {
		return 0, err
	}
	den, err := mulCheck2(from.Num, to.Den)
	if err != nil {
		return 0, err
	}
	factor, err := New(num, den)
	if err != nil {
		return 0, err
	}
	return factor.Scale(v)
}

// UnwrapRTP 依据上一个已展开时间戳 prev，把 32 位回绕的 RTP 时间戳
// cur 展开为 int64 单调序列。向后跳转（如重排或负偏移）会得到负差值，
// 调用方可据此表达负时间戳样本。
func UnwrapRTP(prev int64, cur uint32) int64 {
	delta := int32(uint32(cur) - uint32(prev))
	return prev + int64(delta)
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs64(v int64) int64 {
	if v < 0 {
		if v == math.MinInt64 {
			return math.MaxInt64
		}
		return -v
	}
	return v
}

func mulCheck(a, b int64) int64 {
	r, err := mulCheck2(a, b)
	if err != nil {
		panic(err)
	}
	return r
}

func mulCheck2(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if abs64(a) > math.MaxInt64/abs64(b) {
		return 0, ErrOverflow
	}
	return a * b, nil
}
