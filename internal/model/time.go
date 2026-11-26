// Package model 定义归档媒体内核共用的有理时基、统一时间类型与可分类错误。
//
// 设计约束：
//   - 所有子系统（时间线、切分、静音、指纹、清单、核验）只通过 MediaTime
//     交换时间，禁止各轨独立累计时间；
//   - MediaTime 以微秒为单位的整数时间，避免浮点漂移，保证重复运行产物一致；
//   - Rational 负责把轨道时基（采样率、帧率、封装 time_base）无损换算到 MediaTime。
package model

import (
	"errors"
	"math"
)

// MicrosPerSecond 统一时间分辨率：1 秒 = 1_000_000 微秒。
const MicrosPerSecond int64 = 1_000_000

// MediaTime 是全内核唯一的时间表示：相对素材归一化零点的微秒偏移。
// 负值合法，用于表达负起点素材；回绕时间戳在进入时间线归一前必须先被展开。
type MediaTime int64

// Microseconds 返回原始微秒值。
func (t MediaTime) Microseconds() int64 { return int64(t) }

// Seconds 返回浮点秒数，仅用于展示与核验统计，不得参与累计计算。
func (t MediaTime) Seconds() float64 { return float64(t) / float64(MicrosPerSecond) }

// Add / Sub 是唯一允许的时间位移运算，集中在此处便于审计。
func (t MediaTime) Add(d MediaTime) MediaTime   { return t + d }
func (t MediaTime) Sub(o MediaTime) MediaTime   { return t - o }
func (t MediaTime) Before(o MediaTime) bool     { return t < o }
func (t MediaTime) After(o MediaTime) bool      { return t > o }
func (t MediaTime) Equals(o MediaTime) bool     { return t == o }
func (t MediaTime) IsZero() bool                { return t == 0 }

// Duration 与 MediaTime 同构，语义上表示时间跨度，禁止为负。
type Duration int64

// DurationOf 由起止时间求跨度；若 end < start 返回 0 并由调用方决定是否记错误。
func DurationOf(start, end MediaTime) Duration {
	if end <= start {
		return 0
	}
	return Duration(end - start)
}

// Micros 直接以微秒构造 Duration。
func Micros(v int64) Duration { return Duration(v) }

// Microseconds 返回跨度微秒数。
func (d Duration) Microseconds() int64 { return int64(d) }

// Seconds 仅用于展示。
func (d Duration) Seconds() float64 { return float64(d) / float64(MicrosPerSecond) }

// Rational 是有理时基 num/den，与 FFmpeg AVRational 同构。
// den 必须为正；零值视为非法时基。
type Rational struct {
	Num int64
	Den int64
}

// NewRational 构造有理时基并拒绝非正分母。
func NewRational(num, den int64) (Rational, error) {
	if den <= 0 {
		return Rational{}, errors.New("rational: denominator must be positive")
	}
	return Rational{Num: num, Den: den}, nil
}

// MustRational 用于测试与编译期常量场景，非法时基直接 panic。
func MustRational(num, den int64) Rational {
	r, err := NewRational(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

// Float64 仅用于展示。
func (r Rational) Float64() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// TicksToTime 把轨道时基下的刻度值无损换算为统一微秒时间。
// 换算采用先乘后除，中间值在 int64 范围内（微秒分辨率下约 29 万年）。
func TicksToTime(ticks int64, tb Rational) MediaTime {
	if tb.Den == 0 {
		return 0
	}
	return MediaTime(ticks * tb.Num * MicrosPerSecond / tb.Den)
}

// TimeToTicks 把统一微秒时间换算回指定时基的刻度（向下取整）。
func TimeToTicks(t MediaTime, tb Rational) int64 {
	if tb.Num == 0 || tb.Den == 0 {
		return 0
	}
	return int64(t) * tb.Den / (tb.Num * MicrosPerSecond)
}

// Rounding 是剪辑点落在帧/采样中间时的取整策略。
type Rounding int

const (
	// RoundNearest 四舍五入到最近刻度，等距时向远离零点方向取整。
	RoundNearest Rounding = iota
	// RoundFloor 向时间减小方向取整（不越过剪辑点之后的内容）。
	RoundFloor
	// RoundCeil 向时间增大方向取整。
	RoundCeil
)

// SnapToTicks 把任意 MediaTime 吸附到给定时基的整数刻度。
// 返回吸附后的时间与有符号误差（snap - original，微秒），误差必须留痕。
func SnapToTicks(t MediaTime, tb Rational, mode Rounding) (MediaTime, MediaTime) {
	if tb.Num == 0 || tb.Den == 0 {
		return t, 0
	}
	// 每刻度的微秒数（有理数）：tickMicros = Num*1e6/Den
	// 逆运算 ticks = raw * Den / (Num*1e6)
	num := tb.Num * MicrosPerSecond
	den := tb.Den
	raw := int64(t)
	var ticks int64
	switch mode {
	case RoundFloor:
		ticks = floorDiv(raw*den, num)
	case RoundCeil:
		ticks = floorDiv(raw*den, num)
		if raw*den%num != 0 {
			ticks++
		}
	default: // RoundNearest，等距远离零点
		adj := raw * den
		if raw >= 0 {
			adj += num / 2
		} else {
			adj -= num / 2
		}
		ticks = floorDiv(adj, num)
	}
	snapped := MediaTime(ticks * num / den)
	return snapped, snapped - t
}

// ClampDuration 把 d 限制在 [lo, hi]。
func ClampDuration(d, lo, hi Duration) Duration {
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}

// AbsMicros 返回时间偏差的绝对值。
func AbsMicros(d MediaTime) MediaTime { return MediaTime(abs64(int64(d))) }

// MaxTime / MinTime 供时间线合并使用。
func MaxTime(a, b MediaTime) MediaTime {
	if a > b {
		return a
	}
	return b
}
func MinTime(a, b MediaTime) MediaTime {
	if a < b {
		return a
	}
	return b
}

func floorDiv(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func sign64(v int64) int64 {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	default:
		return 0
	}
}

// Int64Saturate 防止换算溢出，超界时钳到 int64 端点。
func Int64Saturate(v float64) int64 {
	if v >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	if v <= float64(math.MinInt64) {
		return math.MinInt64
	}
	return int64(v)
}
