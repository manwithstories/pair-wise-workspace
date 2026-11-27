// Package media 定义跨子系统共用的媒体时钟与故障类型。
// MediaTime 是 rtp、clock、index、audio 与 engine 共用的统一时间轴，
// Fault 携带包序号与字节偏移，便于值守人员精确定位坏包。
package media

import (
	"math"
	"math/bits"
)

// 常用 RTP 时钟基率（Hz）。
const (
	RateAudio8k   = 8000
	RateAudio16k  = 16000
	RateAudio48k  = 48000
	RateAudio96k  = 96000
	RateVideo90k  = 90000
	RateMillisec  = 1000
	RateMicrosec  = 1_000_000
	RateNanosec   = 1_000_000_000
	// MaxPPM 是时钟恢复允许修正的最大漂移：±200 ppm。
	MaxPPM = 200
)

// MediaTime 表示统一时间轴上的一个时刻。
// Ticks 是可超过 32 位的扩展计数（已处理 RTP 回绕），Rate 为每秒 tick 数。
type MediaTime struct {
	Ticks int64
	Rate  int64
}

// New 构造 MediaTime，Rate 必须为正。
func New(ticks, rate int64) MediaTime {
	if rate <= 0 {
		rate = 1
	}
	return MediaTime{Ticks: ticks, Rate: rate}
}

// ToNanos 换算为纳秒时间轴，采用整数运算避免累积浮点误差。
func (m MediaTime) ToNanos() int64 {
	if m.Rate <= 0 {
		return 0
	}
	return m.Ticks * RateNanosec / m.Rate
}

// To 把当前时刻重采样到另一时钟基率。
func (m MediaTime) To(rate int64) MediaTime {
	if m.Rate <= 0 || rate <= 0 {
		return MediaTime{Rate: rate}
	}
	return MediaTime{Ticks: m.Ticks * rate / m.Rate, Rate: rate}
}

// Sub 返回以秒为单位的时间差（m-other），保留亚秒精度。
func (m MediaTime) Sub(other MediaTime) float64 {
	a := m.ToNanos()
	b := other.ToNanos()
	return float64(a-b) / float64(RateNanosec)
}

// Extend32 将新到达的 32 位 RTP 时间戳扩展为 64 位。
// prev 是上一次扩展结果；依据半圈原则选择回绕方向，支持乱序到达。
func Extend32(prev int64, wrapped uint32) int64 {
	const mod = int64(1) << 32
	const half = mod / 2
	base := prev &^ (mod - 1)
	cand := base | int64(wrapped)
	delta := cand - prev
	if delta > half {
		cand -= mod
	} else if delta < -half {
		cand += mod
	}
	return cand
}

// ScalePPM 按 ppm（百万分率）漂移修正 tick 计数。
// 正值表示发送端偏快、需要压缩；结果四舍五入。
// |ppm| 不得超过 MaxPPM，超出会被钳制。
func ScalePPM(ticks int64, ppm int) int64 {
	if ppm > MaxPPM {
		ppm = MaxPPM
	} else if ppm < -MaxPPM {
		ppm = -MaxPPM
	}
	// 先乘后除，分子加 500000 实现带符号四舍五入。
	num := ticks * int64(1_000_000-ppm)
	if num >= 0 {
		return (num + 500_000) / 1_000_000
	}
	return (num - 500_000) / 1_000_000
}

// SeqDistance 计算两个 16 位 RTP 序号的有向距离（a-b），范围 [-32768,32767]。
// 正确处理 16 位回绕，是重排序与迟到判定的基础。
func SeqDistance(a, b uint16) int {
	d := uint16(a - b)
	if d >= 1<<15 {
		return int(d) - (1 << 16)
	}
	return int(d)
}

// Wrap16 在 16 位序号空间上前进 n 步。
func Wrap16(seq uint16, n int) uint16 {
	return uint16(int(seq) + n)
}

// Abs 返回整数绝对值；MinInt64 保持为 MinInt64（无法表示其正值）。
func Abs(x int64) int64 {
	if x >= 0 {
		return x
	}
	if x == math.MinInt64 {
		return x
	}
	return -x
}

// LeadingZeros32 转发 bits.LeadingZeros32，供子系统统一引用。
func LeadingZeros32(x uint32) int { return bits.LeadingZeros32(x) }
