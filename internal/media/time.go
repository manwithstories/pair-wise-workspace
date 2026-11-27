// Package media 提供包、时间线与音频模块共用的有理数媒体时基原语。
//
// 所有时间戳均以 (分子, 分母) 有理数表示，避免浮点误差在长媒资恢复中累积。
// 本包不依赖墙钟时间，全部运算确定可复现。
package media

import (
	"fmt"
	"math/big"
)

// Rational 表示有理数时间戳 Num/Den。Den 必须为正，Num 可为负。
type Rational struct {
	Num int64
	Den int64
}

// TimeBase 是时基的语义别名：一个刻度对应 1/Den 秒。
type TimeBase = Rational

// 常用时基。
var (
	TBMillis = Rational{Num: 1, Den: 1_000}
	TBMicros = Rational{Num: 1, Den: 1_000_000}
	// TBAudio48k 是归档输出 48 kHz 音频的本地时基。
	TBAudio48k = Rational{Num: 1, Den: 48_000}
)

// NewRational 构造有理数并校验分母为正。
func NewRational(num, den int64) (Rational, error) {
	if den <= 0 {
		return Rational{}, fmt.Errorf("media: 非正分母 %d", den)
	}
	return Rational{Num: num, Den: den}, nil
}

// Valid 报告时基是否合法。
func (r Rational) Valid() bool { return r.Den > 0 }

// Seconds 返回浮点秒数，仅用于展示与测试断言，不参与流水线运算。
func (r Rational) Seconds() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// Millis 四舍五入到毫秒刻度，用于终端报告与检查点摘要。
func (r Rational) Millis() int64 {
	return r.Scale(1_000)
}

// Scale 将时间戳换算为以 base 为分母的整数刻度（四舍五入）。
func (r Rational) Scale(base int64) int64 {
	if r.Den <= 0 || base <= 0 {
		return 0
	}
	num := r.Num * base
	if num >= 0 {
		return (num + r.Den/2) / r.Den
	}
	return -((-num + r.Den/2) / r.Den)
}

// ToSamples 是 Scale 的语义化别名：返回给定采样率下的采样点数。
func (r Rational) ToSamples(sampleRate int64) int64 { return r.Scale(sampleRate) }

// Compare 比较两个有理数：-1 / 0 / 1。
func (r Rational) Compare(o Rational) int {
	// 交叉相乘，两侧分母均为正。
	l := big.NewInt(r.Num)
	l.Mul(l, big.NewInt(o.Den))
	rr := big.NewInt(o.Num)
	rr.Mul(rr, big.NewInt(r.Den))
	return l.Cmp(rr)
}

// Equal 报告两个时间戳是否相等。
func (r Rational) Equal(o Rational) bool { return r.Compare(o) == 0 }

// Add 返回 r+o，结果保持 o 的分母（用于相同时基内累加）。
func (r Rational) Add(o Rational) Rational {
	if r.Den != o.Den {
		// 退化为大整数精确通分。
		d := big.NewInt(r.Den)
		d.Mul(d, big.NewInt(o.Den))
		n1 := big.NewInt(r.Num)
		n1.Mul(n1, big.NewInt(o.Den))
		n2 := big.NewInt(o.Num)
		n2.Mul(n2, big.NewInt(r.Den))
		n1.Add(n1, n2)
		return normalizeBig(n1, d)
	}
	return Rational{Num: r.Num + o.Num, Den: r.Den}
}

// Sub 返回 r-o，规则同 Add。
func (r Rational) Sub(o Rational) Rational {
	neg := Rational{Num: -o.Num, Den: o.Den}
	return r.Add(neg)
}

// Rescale 将时间戳换算到新时基 tb 下（四舍五入）。
func (r Rational) Rescale(tb TimeBase) Rational {
	return Rational{Num: r.Scale(tb.Den), Den: tb.Den}
}

// String 实现 fmt.Stringer。
func (r Rational) String() string {
	if r.Den == 0 {
		return "Rational(<invalid>)"
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

// SamplesToTime 把采样点数转换为指定采样率下的时间戳。
func SamplesToTime(samples, sampleRate int64) Rational {
	if sampleRate <= 0 {
		return Rational{}
	}
	return Rational{Num: samples, Den: sampleRate}
}

func normalizeBig(n, d *big.Int) Rational {
	// 约分，保持分母为正。
	g := new(big.Int).GCD(nil, nil, new(big.Int).Abs(n), d)
	n.Quo(n, g)
	d.Quo(d, g)
	if d.Sign() < 0 {
		n.Neg(n)
		d.Neg(d)
	}
	if !d.IsInt64() || !n.IsInt64() {
		// 媒体时间戳不会溢出 int64；溢出属于编程错误。
		panic("media: 有理数控件溢出 int64")
	}
	return Rational{Num: n.Int64(), Den: d.Int64()}
}
