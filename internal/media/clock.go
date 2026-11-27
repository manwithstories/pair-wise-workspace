package media

// TimeNS 是统一时间基准：自某一现场纪元起的纳秒整数。
// 全系统（时间线、映射、转封装、检查点）只使用该整数时间，不使用 wall clock。
type TimeNS int64

// 常用纳秒换算常量。
const (
	Nanosecond  TimeNS = 1
	Microsecond TimeNS = 1_000
	Millisecond TimeNS = 1_000_000
	Second      TimeNS = 1_000_000_000
)

// GraceWindow 是水位相对最大事件时间的固定滞后量（250ms）。
const GraceWindow TimeNS = 250 * Millisecond

// FromMillis 将毫秒时间戳转换为纳秒基准。
func FromMillis(ms int64) TimeNS { return TimeNS(ms) * Millisecond }

// FromMicros 将微秒时间戳转换为纳秒基准。
func FromMicros(us int64) TimeNS { return TimeNS(us) * Microsecond }

// Millis 返回毫秒表示（截断）。
func (t TimeNS) Millis() int64 { return int64(t / Millisecond) }

// Add 返回偏移后的时间。
func (t TimeNS) Add(d TimeNS) TimeNS { return t + d }
