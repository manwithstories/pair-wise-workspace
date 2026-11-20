// Package ingest 流式解析字幕与时间码，保留输入偏移并输出统一时间事件。
package ingest

import (
	"strconv"
	"strings"

	"mediamux/media"
)

// maxClockHours 是时钟制时间码的小时上限，超出视为溢出。
const maxClockHours = 100000

// ParseTimecode 解析多制式时间码为统一时基刻度：
//   - 帧号制 "HH:MM:SS:FF"（按 fr 换算，校验帧号上限与日界回绕）
//   - 时钟制 "[HH:]MM:SS.mmm" 或 "[HH:]MM:SS,mmm"（SRT 逗号、VTT 点号）
//
// 支持可选前导 "-" 负偏移。非法帧号、回绕歧义（HH>24）与溢出
// 均以携带原始偏移的可恢复错误报告。
func ParseTimecode(s string, fr media.FrameRate, offset int64) (media.Ticks, error) {
	s = strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	var ticks media.Ticks
	var err *media.Error
	if strings.ContainsAny(s, ".,") {
		ticks, err = parseClock(s, offset)
	} else {
		ticks, err = parseFrames(s, fr, offset)
	}
	if err != nil {
		return 0, err
	}
	if neg {
		ticks = -ticks
	}
	return ticks, nil
}

// parseFrames 解析帧号制 "HH:MM:SS:FF"。
func parseFrames(s string, fr media.FrameRate, offset int64) (media.Ticks, *media.Error) {
	fields := strings.Split(s, ":")
	if len(fields) != 4 {
		return 0, media.New(media.StageTimecode, "sub", offset, true,
			"时间码格式非法（应为 HH:MM:SS:FF）: %q", s)
	}
	v := make([]int64, 4)
	for i, f := range fields {
		n, ok := atoi(f)
		if !ok {
			return 0, media.New(media.StageTimecode, "sub", offset, true,
				"时间码分量非数字: %q", f)
		}
		v[i] = n
	}
	h, m, sec, ff := v[0], v[1], v[2], v[3]
	if m >= 60 || sec >= 60 {
		return 0, media.New(media.StageTimecode, "sub", offset, true,
			"时间码分量越界: %q", s)
	}
	if h > 24 || (h == 24 && (m != 0 || sec != 0 || ff != 0)) {
		return 0, media.New(media.StageTimecode, "sub", offset, true,
			"日界回绕歧义: HH=%d", h)
	}
	if fps := fr.WholeFPS(); ff >= fps {
		return 0, media.New(media.StageTimecode, "sub", offset, true,
			"非法帧号 %d（帧率上限 %d）", ff, fps)
	}
	frames := ((h*60+m)*60+sec)*fr.WholeFPS() + ff
	return fr.FramesToTicks(frames), nil
}

// parseClock 解析时钟制 "[HH:]MM:SS.mmm"。
func parseClock(s string, offset int64) (media.Ticks, *media.Error) {
	msPart := ""
	if i := strings.IndexAny(s, ".,"); i >= 0 {
		msPart = s[i+1:]
		s = s[:i]
	}
	if len(msPart) == 0 || len(msPart) > 3 {
		return 0, media.New(media.StageTimecode, "sub", offset, true,
			"毫秒位数非法: %q", msPart)
	}
	ms, ok := atoi(msPart)
	if !ok {
		return 0, media.New(media.StageTimecode, "sub", offset, true,
			"毫秒分量非数字: %q", msPart)
	}
	for i := len(msPart); i < 3; i++ {
		ms *= 10 // 不足 3 位按右侧补零
	}
	fields := strings.Split(s, ":")
	if len(fields) < 2 || len(fields) > 3 {
		return 0, media.New(media.StageTimecode, "sub", offset, true,
			"时间码格式非法（应为 [HH:]MM:SS.mmm）: %q", s)
	}
	var h, m, sec int64
	vals := make([]int64, len(fields))
	for i, f := range fields {
		n, ok := atoi(f)
		if !ok {
			return 0, media.New(media.StageTimecode, "sub", offset, true,
				"时间码分量非数字: %q", f)
		}
		vals[i] = n
	}
	if len(vals) == 3 {
		h, m, sec = vals[0], vals[1], vals[2]
	} else {
		m, sec = vals[0], vals[1]
	}
	if m >= 60 || sec >= 60 {
		return 0, media.New(media.StageTimecode, "sub", offset, true,
			"时间码分量越界: %q", s)
	}
	if h > maxClockHours {
		return 0, media.New(media.StageTimecode, "sub", offset, true,
			"时间码溢出: HH=%d 超过上限 %d", h, maxClockHours)
	}
	total := ((h*60+m)*60+sec)*1000 + ms
	return media.MillisToTicks(total), nil
}

// atoi 解析非负十进制整数，拒绝空串、符号与溢出。
func atoi(s string) (int64, bool) {
	if s == "" || len(s) > 15 {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
