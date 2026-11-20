// Package timeline 提供统一时间基准：整数刻度、有理帧率与多制式时间码换算。
// 所有模块共享该基准，避免浮点漂移；时间戳一律使用 int64 刻度。
package timeline

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MasterTimescale 为主时间基准（刻度/秒）。
// 27000000 可被 24、25、30、30000/1001、90000 整除，保证帧边界为整数刻度。
const MasterTimescale int64 = 27000000

// TicksPerDay 为 24 小时对应的刻度数，用于日界回绕。
const TicksPerDay = MasterTimescale * 86400

// Rate 表示有理帧率（Num/Den 帧每秒）。
type Rate struct {
	Num int32
	Den int32
}

// 常用制式帧率。
var (
	Rate24   = Rate{Num: 24, Den: 1}
	Rate25   = Rate{Num: 25, Den: 1}
	Rate30   = Rate{Num: 30, Den: 1}
	RateNTSC = Rate{Num: 30000, Den: 1001} // 29.97 fps
)

// TimecodeError 记录非法时间码及其原始文本，便于溯源。
type TimecodeError struct {
	Raw    string
	Reason string
}

func (e *TimecodeError) Error() string {
	return fmt.Sprintf("invalid timecode %q: %s", e.Raw, e.Reason)
}

// FrameDuration 返回单帧对应的主基准刻度数；不能整除时报错。
func (r Rate) FrameDuration() (int64, error) {
	if r.Num <= 0 || r.Den <= 0 {
		return 0, fmt.Errorf("invalid rate %d/%d", r.Num, r.Den)
	}
	num := MasterTimescale * int64(r.Den)
	if num%int64(r.Num) != 0 {
		return 0, fmt.Errorf("rate %d/%d not representable in master timescale", r.Num, r.Den)
	}
	return num / int64(r.Num), nil
}

// RoundFPS 返回帧号取整后的每秒帧数（NTSC 按 30 计）。
func (r Rate) RoundFPS() int64 {
	return (int64(r.Num)*2 + int64(r.Den)) / (2 * int64(r.Den))
}

// FramesPerDay 返回 24 小时内的帧数（按取整帧率计）。
func (r Rate) FramesPerDay() int64 {
	return r.RoundFPS() * 86400
}

// ParseTimecode 解析 "HH:MM:SS:FF" 时间码，支持前导负号（负偏移）。
// 超过 24 小时的正向时间码按日界回绕归一化并返回回绕次数；
// 帧号、分、秒越界或刻度溢出时返回 *TimecodeError 并保留原始文本。
func ParseTimecode(raw string, rate Rate) (ticks int64, wraps int, err error) {
	s := strings.TrimSpace(raw)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) != 4 {
		return 0, 0, &TimecodeError{Raw: raw, Reason: "expect HH:MM:SS:FF"}
	}
	v := make([]int64, 4)
	for i, p := range parts {
		n, perr := strconv.ParseInt(p, 10, 64)
		if perr != nil || n < 0 {
			return 0, 0, &TimecodeError{Raw: raw, Reason: "non-numeric or negative field"}
		}
		v[i] = n
	}
	if v[1] >= 60 || v[2] >= 60 {
		return 0, 0, &TimecodeError{Raw: raw, Reason: "minute/second out of range"}
	}
	fps := rate.RoundFPS()
	if v[3] >= fps {
		return 0, 0, &TimecodeError{Raw: raw, Reason: "frame number out of range"}
	}
	frames := ((v[0]*60+v[1])*60+v[2])*fps + v[3]
	fpd := rate.FramesPerDay()
	if frames > 1<<50 { // 拒绝荒谬长度，防溢出
		return 0, 0, &TimecodeError{Raw: raw, Reason: "timecode too large"}
	}
	if neg {
		ticks, err = FramesToTicks(-frames, rate)
		return ticks, 0, err
	}
	wraps = int(frames / fpd)
	frames %= fpd
	ticks, err = FramesToTicks(frames, rate)
	return ticks, wraps, err
}

// FramesToTicks 将帧号（可为负）换算为主基准刻度，溢出时报错。
func FramesToTicks(frames int64, rate Rate) (int64, error) {
	d, err := rate.FrameDuration()
	if err != nil {
		return 0, err
	}
	if frames > (1<<62)/d || frames < -(1<<62)/d {
		return 0, errors.New("tick overflow")
	}
	return frames * d, nil
}

// PTSToTicks 将任意时基的 PTS 换算为主基准刻度，带溢出检查。
func PTSToTicks(pts int64, timescale uint32) (int64, error) {
	if timescale == 0 {
		return 0, errors.New("zero timescale")
	}
	ts := int64(timescale)
	if pts != 0 && (pts > (1<<62)/MasterTimescale || pts < -(1<<62)/MasterTimescale) {
		return 0, fmt.Errorf("pts %d overflows master timescale", pts)
	}
	return pts * MasterTimescale / ts, nil
}

// TicksToMillis 将主基准刻度换算为毫秒（向零取整）。
func TicksToMillis(t int64) int64 {
	return t * 1000 / MasterTimescale
}

// MillisToTicks 将毫秒换算为主基准刻度。
func MillisToTicks(ms int64) int64 {
	return ms * (MasterTimescale / 1000)
}

// FormatTimecode 将刻度格式化为 "HH:MM:SS:FF"（按给定帧率，模 24 小时）。
func FormatTimecode(ticks int64, rate Rate) string {
	d, err := rate.FrameDuration()
	if err != nil || d == 0 {
		return "invalid"
	}
	frames := ticks / d
	fps := rate.RoundFPS()
	fpd := rate.FramesPerDay()
	frames %= fpd
	if frames < 0 {
		frames += fpd
	}
	f := frames % fps
	sec := frames / fps
	return fmt.Sprintf("%02d:%02d:%02d:%02d", sec/3600, (sec/60)%60, sec%60, f)
}
