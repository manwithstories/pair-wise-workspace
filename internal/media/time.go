// Package media 定义全管线共用的纳秒时基与时间戳换算工具。
//
// 所有模块对外暴露的时间均为有符号纳秒（int64），音频 48 kHz、
// 视频 90 kHz 的 RTP 时钟在进入管线时统一换算到该时基。
package media

import "math"

// RTP/Matroska 常用时钟频率与时间刻度。
const (
	AudioClockRate uint32 = 48000 // 音频 RTP 时钟（Hz）
	VideoClockRate uint32 = 90000 // 视频 RTP 时钟（Hz）

	NSPerSecond     int64 = 1_000_000_000
	MKTimecodeScale int64 = 1_000_000 // Matroska 默认 TimecodeScale：1 ms

	// TrackNone 表示错误不隶属于任何具体轨道。
	TrackNone uint32 = 0
	// TrackAudio / TrackVideo 为夹具生成的固定轨道号。
	TrackAudio uint32 = 1
	TrackVideo uint32 = 2
)

// SamplesToNS 将指定时钟下的采样数（可为负）换算为有符号纳秒。
func SamplesToNS(clockRate uint32, samples int64) int64 {
	if clockRate == 0 {
		return 0
	}
	return samples * NSPerSecond / int64(clockRate)
}

// NSToSamples 将有符号纳秒换算回指定时钟下的采样数（向零取整）。
func NSToSamples(clockRate uint32, ns int64) int64 {
	if clockRate == 0 {
		return 0
	}
	return ns * int64(clockRate) / NSPerSecond
}

// MKTimecodeToNS 将 Matroska 毫秒时间码（集群时间码 + 块相对时间码）
// 换算为纳秒。
func MKTimecodeToNS(timecodeMS int64) int64 {
	return timecodeMS * MKTimecodeScale
}

// Extend32 依据上一个已展开的 32 位值，把当前 32 位 RTP 时间戳展开为
// int64。返回值可能为负（对端真的回退时间戳时），调用方负责甄别。
func Extend32(prevUnwrapped int64, current uint32) int64 {
	delta := int64(int32(current - uint32(prevUnwrapped)))
	return prevUnwrapped + delta
}

// Extend16 依据上一个已展开的 16 位值，把当前 RTP 序列号展开为 int64。
func Extend16(prevUnwrapped int64, current uint16) int64 {
	delta := int64(int16(current - uint16(prevUnwrapped)))
	return prevUnwrapped + delta
}

// Clock 跟踪单条 RTP 轨道的 32 位时间戳展开状态，识别回绕与回退。
type Clock struct {
	Rate    uint32
	started bool
	lastRaw uint32
	// Last 为最近一次展开后的采样数（int64，可为负）。
	Last int64
	// Wraps 统计正向回绕（2^32 边界跨越）次数。
	Wraps int
	// Backwards 统计时间戳回退（负时间戳）次数。
	Backwards int
}

// NewClock 创建指定频率的轨道时钟。
func NewClock(rate uint32) *Clock { return &Clock{Rate: rate} }

// Update 输入一个原始 RTP 时间戳，返回展开后的采样数、纳秒时间戳，
// 以及本次是否发生正向回绕 / 负向回退。首次输入仅建立基准。
func (c *Clock) Update(rtpTS uint32) (samples int64, ns int64, wrapped bool, backward bool) {
	if !c.started {
		c.started = true
		c.lastRaw = rtpTS
		c.Last = int64(rtpTS)
		return c.Last, SamplesToNS(c.Rate, c.Last), false, false
	}
	delta := int64(int32(rtpTS - c.lastRaw))
	wrapped = delta > 0 && rtpTS < c.lastRaw
	backward = delta < 0
	if wrapped {
		c.Wraps++
	}
	if backward {
		c.Backwards++
	}
	c.lastRaw = rtpTS
	c.Last += delta
	return c.Last, SamplesToNS(c.Rate, c.Last), wrapped, backward
}

// AbsNS 是统计口径下的绝对值保护，避免极端负值在取毫秒时溢出。
func AbsNS(ns int64) int64 {
	if ns == math.MinInt64 {
		return math.MaxInt64
	}
	if ns < 0 {
		return -ns
	}
	return ns
}
