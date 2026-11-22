// Package clock 定义媒体管线统一的采样刻度，以及 RTP 32 位时间戳到
// 单调样本计数的换算与回绕规则。所有阶段共享同一刻度，避免各自引入时间基。
package clock

import (
	"errors"
	"math"
)

// 常用媒体采样率。
const (
	RatePCM8k    uint32 = 8000
	RatePCM16k   uint32 = 16000
	RatePCM48k   uint32 = 48000
	RateVideo90k uint32 = 90000 // RTP 视频约定时钟
)

// MaxUint32 是 RTP 32 位时间戳/序号的模数。
const MaxUint32 = uint64(1) << 32

// Clock 绑定一个采样率，负责在统一的 uint64 样本刻度上做换算。
// 零值不可用，请使用 New 构造。
type Clock struct {
	rate uint32
}

// New 构造采样率为 rate 的刻度。rate 必须为正。
func New(rate uint32) (Clock, error) {
	if rate == 0 {
		return Clock{}, errors.New("clock: sample rate must be non-zero")
	}
	return Clock{rate: rate}, nil
}

// MustNew 与 New 相同，但在非法参数时 panic，适合包级常量初始化。
func MustNew(rate uint32) Clock {
	c, err := New(rate)
	if err != nil {
		panic(err)
	}
	return c
}

// Rate 返回绑定的采样率。
func (c Clock) Rate() uint32 { return c.rate }

// Unwrap 把新到达的 32 位 RTP 时间戳 ts 换算为单调的 64 位样本计数，
// 以 prev 为上一已解包值。规则（RFC 3550 序号差半区约定）：
//   - 首次调用（prev 未知）以 prevIsValid=false 传入，直接零基展开；
//   - 模 2^32 的无符号差值落在 [1, 2^31-1] 视为前进（含跨越 0 的回绕），
//     落在 [2^31, 2^32-1] 视为时间戳回退；
//   - 全程 uint64 运算，天然规避 32 位溢出。
func Unwrap(prev uint64, prevIsValid bool, ts uint32) uint64 {
	if !prevIsValid {
		return uint64(ts)
	}
	ref := uint32(prev % MaxUint32)
	// 32 位有符号差值：正向落在 [1,2^31-1]，回退落在 [2^31,2^32-1]。
	delta := int64(int32(ts - ref))
	// uint64(int64) 的转换对负值自动按 2^64 回绕，差值仍然精确。
	return uint64(int64(prev) + delta)
}

// ToMillis 把样本刻度值转换为毫秒，结果四舍五入。
func (c Clock) ToMillis(samples uint64) uint64 {
	return (samples*1000 + uint64(c.rate)/2) / uint64(c.rate)
}

// ToMillisDiff 计算两个样本刻度之间的有符号毫秒差（b-a）。
func (c Clock) ToMillisDiff(a, b uint64) int64 {
	d := int64(b) - int64(a)
	if d >= 0 {
		return int64((uint64(d)*1000 + uint64(c.rate)/2) / uint64(c.rate))
	}
	return -int64((uint64(-d)*1000 + uint64(c.rate)/2) / uint64(c.rate))
}

// MillisToSamples 把毫秒时长换算为样本数（四舍五入）。
func (c Clock) MillisToSamples(millis uint64) uint64 {
	return (millis*uint64(c.rate) + 500) / 1000
}

// Convert 把另一个刻度 src 上的样本值转换到当前刻度（四舍五入）。
// 中间乘法走 CheckedMul，溢出时返回错误。
func (c Clock) Convert(samples uint64, src Clock) (uint64, error) {
	if src.rate == c.rate {
		return samples, nil
	}
	num, err := CheckedMul(samples, uint64(c.rate))
	if err != nil {
		return 0, err
	}
	return (num + uint64(src.rate)/2) / uint64(src.rate), nil
}

// CheckedMul 计算 a*b，溢出时返回错误而不是静默回绕。
func CheckedMul(a, b uint64) (uint64, error) {
	if a != 0 && b > math.MaxUint64/a {
		return 0, errors.New("clock: integer overflow in multiplication")
	}
	return a * b, nil
}

// CheckedAdd 计算 a+b，溢出时返回错误。
func CheckedAdd(a, b uint64) (uint64, error) {
	if b > math.MaxUint64-a {
		return 0, errors.New("clock: integer overflow in addition")
	}
	return a + b, nil
}
