// Package clock 定义全流水线共用的有理数采样/帧时基。
//
// 所有时间换算都基于精确有理数，避免浮点累计误差；当换算无法整除时，
// 由调用方通过显式取整规则处理并把残差计入时间对齐误差。
package clock

import "fmt"

// Rational 表示精确有理数 num/den，约定 den > 0 且已约分。
type Rational struct {
	Num int64
	Den int64
}

// NewRational 构造并约分一个有理数，分母必须为正。
func NewRational(num, den int64) (Rational, error) {
	if den <= 0 {
		return Rational{}, fmt.Errorf("clock: 非正分母 %d", den)
	}
	if num == 0 {
		return Rational{Num: 0, Den: 1}, nil
	}
	g := gcd(abs64(num), den)
	return Rational{Num: num / g, Den: den / g}, nil
}

// MustRational 是 NewRational 的必成功版本，仅用于常量级入参。
func MustRational(num, den int64) Rational {
	r, err := NewRational(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

// MulInt64 返回 r*x 的精确值；仅当结果整除时无误差。
func (r Rational) MulInt64(x int64) int64 {
	return r.Num * x / r.Den
}

// Equal 判断两个有理数是否相等。
func (r Rational) Equal(o Rational) bool {
	return r.Num == o.Num && r.Den == o.Den
}

// TimeBase 以“每秒采样数”描述一条音频时间线。
type TimeBase struct {
	Rate int64
}

// NewTimeBase 校验采样率为正。
func NewTimeBase(rate int64) (TimeBase, error) {
	if rate <= 0 {
		return TimeBase{}, fmt.Errorf("clock: 非法采样率 %d", rate)
	}
	return TimeBase{Rate: rate}, nil
}

// SamplesPerFrame 返回 fps 下每帧占多少采样（精确有理数）。
func (tb TimeBase) SamplesPerFrame(fps Rational) Rational {
	return MustRational(tb.Rate*fps.Den, fps.Num)
}

// FrameStart 返回第 frame 帧（0 基）首个采样的全局采样序号。
func (tb TimeBase) FrameStart(frame int64, fps Rational) int64 {
	return frame * tb.Rate * fps.Den / fps.Num
}

// FrameOf 返回全局采样 sample 所属的帧序号（向下取整）。
func (tb TimeBase) FrameOf(sample int64, fps Rational) int64 {
	if sample < 0 {
		return (sample*fps.Num - tb.Rate*fps.Den + 1) / (tb.Rate * fps.Den)
	}
	return sample * fps.Num / (tb.Rate * fps.Den)
}

// FrameCount 返回 totalSamples 个采样在 fps 下覆盖的整帧数。
func (tb TimeBase) FrameCount(totalSamples int64, fps Rational) int64 {
	if totalSamples <= 0 {
		return 0
	}
	return (totalSamples*fps.Num + tb.Rate*fps.Den - 1) / (tb.Rate * fps.Den)
}

// SamplesForDuration 返回时长 d（秒，有理数）对应的采样数（向下取整）。
func (tb TimeBase) SamplesForDuration(d Rational) int64 {
	return d.Num * tb.Rate / d.Den
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
