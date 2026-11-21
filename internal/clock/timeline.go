// Package clock 定义统一的有理数时间基准与时间换算。
// 所有子系统（清单、分片、RTP、音频）共享该基准，避免浮点累积误差。
package clock

import "fmt"

// Rational 表示有理数时间，单位为秒。
type Rational struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// New 构造并规范化有理数；den 为 0 时按 1 处理。
func New(num, den int64) Rational {
	if den == 0 {
		den = 1
	}
	if den < 0 {
		num, den = -num, -den
	}
	if g := gcd(abs(num), den); g > 1 {
		num, den = num/g, den/g
	}
	return Rational{Num: num, Den: den}
}

// SamplesToTime 将采样数换算为时间。
func SamplesToTime(samples, rate int64) Rational { return New(samples, rate) }

// Seconds 返回浮点秒。
func (r Rational) Seconds() float64 { return float64(r.Num) / float64(r.Den) }

// Millis 返回浮点毫秒。
func (r Rational) Millis() float64 { return r.Seconds() * 1000 }

// Add 返回 r+o。
func (r Rational) Add(o Rational) Rational {
	return New(r.Num*o.Den+o.Num*r.Den, r.Den*o.Den)
}

// Sub 返回 r-o。
func (r Rational) Sub(o Rational) Rational {
	return New(r.Num*o.Den-o.Num*r.Den, r.Den*o.Den)
}

// Cmp 比较两个时间：-1/0/1。
func (r Rational) Cmp(o Rational) int {
	l, rr := r.Num*o.Den, o.Num*r.Den
	switch {
	case l < rr:
		return -1
	case l > rr:
		return 1
	}
	return 0
}

// Samples 将时间换算为指定采样率下的采样数（四舍五入）。
func (r Rational) Samples(rate int64) int64 {
	num, den := r.Num*rate, r.Den
	if num >= 0 {
		return (num + den/2) / den
	}
	return -((-num + den/2) / den)
}

func (r Rational) String() string { return fmt.Sprintf("%d/%d", r.Num, r.Den) }

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
