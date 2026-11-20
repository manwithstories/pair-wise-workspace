// Package media 定义全流水线共享的有理数时间基准与错误模型。
//
// 所有模块（字幕、RTP、MP4、同步）都把各自的时间表示换算到
// MediaTimebase（90 kHz，与 RTP 时钟一致）后再比较与统计，
// 避免各自解释时间戳导致的偏差。
package media

import "math/big"

// Timebase 是有理数时间基准：1 个 tick = Num/Den 秒。
type Timebase struct {
	Num int64
	Den int64
}

// Ticks 是某一 Timebase 下的整数刻度，允许负值（负偏移）。
type Ticks int64

// MediaTimebase 是统一时基（1/90000 秒）。
var MediaTimebase = Timebase{Num: 1, Den: 90000}

// Day 返回该时基下 24 小时的刻度数，用于日界回绕处理。
func (tb Timebase) Day() Ticks {
	return Ticks(86400 * tb.Den / tb.Num)
}

// Seconds 返回刻度的浮点秒数，仅用于展示与统计。
func (tb Timebase) Seconds(t Ticks) float64 {
	return float64(t) * float64(tb.Num) / float64(tb.Den)
}

// Millis 返回统一时基刻度的整数毫秒数（向零截断）。
func Millis(t Ticks) int64 {
	return int64(t) * 1000 / MediaTimebase.Den
}

// MillisToTicks 把毫秒换算为统一时基刻度。
func MillisToTicks(ms int64) Ticks {
	return Convert(Ticks(ms), Timebase{1, 1000}, MediaTimebase)
}

// Convert 把 t 从 from 时基换算到 to 时基，向最近刻度舍入。
// 中间量使用 big.Int，任意时基组合与负值都不会溢出。
func Convert(t Ticks, from, to Timebase) Ticks {
	if from == to {
		return t
	}
	num := big.NewInt(int64(t))
	num.Mul(num, big.NewInt(from.Num))
	num.Mul(num, big.NewInt(to.Den))
	den := big.NewInt(from.Den)
	den.Mul(den, big.NewInt(to.Num))
	return Ticks(divRound(num, den))
}

// divRound 计算 num/den（den>0）并向最近整数舍入，半数向远离零方向。
func divRound(num, den *big.Int) int64 {
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(num, den, r) // 向零截断
	rr := new(big.Int).Abs(r)
	rr.Lsh(rr, 1)
	if rr.Cmp(den) >= 0 {
		if num.Sign() >= 0 {
			q.Add(q, big.NewInt(1))
		} else {
			q.Sub(q, big.NewInt(1))
		}
	}
	return q.Int64()
}

// FrameRate 是有理数帧率（帧/秒），如 30000/1001。
type FrameRate struct {
	Num int64
	Den int64
}

// 常见帧率。
var (
	FrameRate24   = FrameRate{24, 1}
	FrameRate25   = FrameRate{25, 1}
	FrameRate30   = FrameRate{30, 1}
	FrameRate2997 = FrameRate{30000, 1001}
)

// FrameDuration 返回单帧在统一时基下的时长。
// 30000/1001 时恰为 3003 刻度，无舍入误差。
func (fr FrameRate) FrameDuration() Ticks {
	return Convert(1, Timebase{fr.Den, fr.Num}, MediaTimebase)
}

// FramesToTicks 把帧计数换算为统一时基刻度。
func (fr FrameRate) FramesToTicks(frames int64) Ticks {
	return Convert(Ticks(frames), Timebase{fr.Den, fr.Num}, MediaTimebase)
}

// WholeFPS 返回时间码帧号上限所用的整数帧率（四舍五入）。
func (fr FrameRate) WholeFPS() int64 {
	return (fr.Num + fr.Den/2) / fr.Den
}

// UnwrapDay 处理日界回绕：prev 为上一事件的统一时基刻度，
// cur 为当日新解析刻度（可能已回绕到 0 附近）。
// 返回与 prev 之差落在 ±半天 内的等值刻度（可超出一天或为负）。
func UnwrapDay(prev, cur Ticks) Ticks {
	day := MediaTimebase.Day()
	half := day / 2
	for cur-prev > half {
		cur -= day
	}
	for prev-cur > half {
		cur += day
	}
	return cur
}
