// Package model 定义各子系统共享的时间基准与数据记录。
//
// 全系统统一使用有理数表示的纳秒时间轴：每个时间点为 num/den 纳秒，
// 全程不使用浮点累加，避免分片时长逐项相加产生的漂移。
package model

import (
	"errors"
	"strconv"
	"strings"
)

// Rat 是以纳秒为单位的有理数时间点或时长。
// 零值（Den==0）表示非法时间，使用前必须经构造函数生成。
type Rat struct {
	Num int64
	Den int64 // 恒为正数
}

// InvalidRat 表示无法确定的时间（如时间尺度为零的分片）。
var InvalidRat = Rat{}

var errDenorm = errors.New("invalid rational time")

// NewRat 以 num/den 纳秒构造有理数时间并约分。
func NewRat(num, den int64) (Rat, error) {
	if den == 0 {
		return InvalidRat, errDenorm
	}
	if den < 0 {
		num, den = -num, -den
	}
	g := gcd(abs64(num), den)
	if g > 1 {
		num /= g
		den /= g
	}
	return Rat{Num: num, Den: den}, nil
}

// Nano 用整数纳秒构造时间点。
func Nano(ns int64) Rat { return Rat{Num: ns, Den: 1} }

// FromTicks 把按 timeScale 计时的媒体 tick 换算为纳秒时间。
// timeScale<=0 时返回非法时间（调用方应登记故障）。
func FromTicks(ticks, timeScale int64) Rat {
	if timeScale <= 0 {
		return InvalidRat
	}
	r, _ := NewRat(ticks*1_000_000_000, timeScale)
	return r
}

// FromDecimalSeconds 解析清单中的十进制秒（如 "9.0"、"1.006"），
// 按位数精确换算为纳秒，不经过浮点。
func FromDecimalSeconds(s string) (Rat, error) {
	s = strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	dot := strings.IndexByte(s, '.')
	var whole, frac string
	if dot < 0 {
		whole = s
	} else {
		whole, frac = s[:dot], s[dot+1:]
	}
	if whole == "" {
		whole = "0"
	}
	if dot >= 0 && frac == "" {
		return InvalidRat, errDenorm
	}
	for _, c := range whole + frac {
		if c < '0' || c > '9' {
			return InvalidRat, errDenorm
		}
	}
	iv, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return InvalidRat, err
	}
	if neg {
		iv = -iv
	}
	den := int64(1)
	for range frac {
		den *= 10
	}
	return NewRat(iv, den)
}

// IsValid 报告时间是否合法。
func (r Rat) IsValid() bool { return r.Den > 0 }

// Add 精确相加，任一操作数非法则结果非法。
func (r Rat) Add(o Rat) Rat {
	if !r.IsValid() || !o.IsValid() {
		return InvalidRat
	}
	g := gcd(r.Den, o.Den)
	den := (r.Den / g) * o.Den
	return mustRat(r.Num*(den/r.Den) + o.Num*(den/o.Den), den)
}

// MulInt 返回 r*n；非法时间参与运算仍为非法。
func (r Rat) MulInt(n int64) Rat {
	if !r.IsValid() {
		return InvalidRat
	}
	return mustRat(r.Num*n, r.Den)
}

// Sub 精确相减，任一操作数非法则结果非法。
func (r Rat) Sub(o Rat) Rat {
	if !r.IsValid() || !o.IsValid() {
		return InvalidRat
	}
	return r.Add(Rat{Num: -o.Num, Den: o.Den})
}

// Cmp 返回 -1/0/1；非法时间比较会 panic，调用方应先检查 IsValid。
func (r Rat) Cmp(o Rat) int {
	ln := r.Num * o.Den
	rn := o.Num * r.Den
	switch {
	case ln < rn:
		return -1
	case ln > rn:
		return 1
	default:
		return 0
	}
}

// RoundNanos 四舍五入到整数纳秒。
func (r Rat) RoundNanos() int64 {
	if !r.IsValid() {
		return 0
	}
	q, rem := r.Num/r.Den, r.Num%r.Den
	if rem < 0 {
		rem = -rem
		q--
	}
	if rem*2 >= r.Den {
		q++
	}
	return q
}

// RoundMillis 四舍五入到整数毫秒，供报表 keyframe_gap_ms 使用。
func (r Rat) RoundMillis() int64 {
	ms, err := NewRat(r.Num, r.Den*1_000_000)
	if err != nil {
		return 0
	}
	return ms.RoundNanos()
}

// String 以小数纳秒形式输出，便于测试与报表。
func (r Rat) String() string {
	if !r.IsValid() {
		return "invalid"
	}
	whole, rem := r.Num/r.Den, r.Num%r.Den
	if rem == 0 {
		return strconv.FormatInt(whole, 10)
	}
	return strconv.FormatInt(r.Num, 10) + "/" + strconv.FormatInt(r.Den, 10)
}

func mustRat(num, den int64) Rat {
	r, err := NewRat(num, den)
	if err != nil {
		panic(err)
	}
	return r
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
