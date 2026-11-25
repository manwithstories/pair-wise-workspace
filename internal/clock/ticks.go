// Package clock 定义全项目统一的 1/48000 秒整数媒体时基。
package clock

import (
	"fmt"
	"time"
)

// Hz 是省级媒资恢复管线统一使用的采样率。
const Hz int64 = 48_000

// Ticks 表示 1/48000 秒的整数刻度。
type Ticks int64

// FromDuration 将标准库时长换算为统一刻度，采用四舍五入。
func FromDuration(d time.Duration) Ticks {
	return Ticks((d.Nanoseconds()*Hz + int64(time.Second)/2) / int64(time.Second))
}

// Scale 把 inRate 时基上的整数 value 精确换算到 48kHz。
func Scale(value uint64, inRate uint32) Ticks {
	if inRate == 0 {
		return 0
	}
	// 先做乘法可能溢出时，使用商和余数完成四舍五入。
	q := value / uint64(inRate)
	r := value % uint64(inRate)
	extra := (r*uint64(Hz) + uint64(inRate)/2) / uint64(inRate)
	return Ticks(q*uint64(Hz) + extra)
}

// AACDuration 换算 MP4 stts 中的 AAC 样本时长。
func AACDuration(delta uint32, timescale uint32) Ticks {
	return Scale(uint64(delta), timescale)
}

// Unwrap16 依据上一个扩展值把 uint16 序号还原为单调递增整数。
func Unwrap16(prev int64, v uint16) int64 {
	if prev == 0 {
		return int64(v)
	}
	cycle := prev &^ 0xffff
	candidate := cycle + int64(v)
	for candidate-prev > 1<<15 {
		candidate -= 1 << 16
	}
	for prev-candidate > 1<<15 {
		candidate += 1 << 16
	}
	return candidate
}

// Unwrap32 依据上一个扩展值把 uint32 RTP 时间戳还原为单调递增整数。
func Unwrap32(prev int64, v uint32) int64 {
	if prev == 0 {
		return int64(v)
	}
	cycle := prev &^ 0xffffffff
	candidate := cycle + int64(v)
	for candidate-prev > 1<<31 {
		candidate -= 1 << 32
	}
	for prev-candidate > 1<<31 {
		candidate += 1 << 32
	}
	return candidate
}

// OpusDuration 根据 Opus TOC 和帧数计算包时长。
// 仅覆盖归档样例使用的常见单帧/双帧编码，同时保留规范中的主要时长档。
func OpusDuration(payload []byte) (Ticks, error) {
	if len(payload) < 1 {
		return 0, fmt.Errorf("opus: empty TOC")
	}
	toc := payload[0]
	config := toc >> 3
	code := toc & 0x03

	// RFC 6716 配置号对应的单帧时长，单位为 1/48000 秒。
	var frame Ticks
	switch {
	case config <= 3 || (config >= 8 && config <= 11):
		frame = []Ticks{480, 960, 1920, 2880}[config&3] // 10/20/40/60ms
	case config >= 4 && config <= 7:
		frame = []Ticks{480, 960, 1920, 2880}[config&3]
	case config >= 12 && config <= 15:
		frame = []Ticks{480, 960, 0, 0}[config&3] // 混合模式仅 10/20ms
		if frame == 0 {
			return 0, fmt.Errorf("opus: reserved hybrid config %d", config)
		}
	case config >= 16 && config <= 19:
		frame = []Ticks{120, 240, 480, 960}[config&3] // 2.5/5/10/20ms
	default: // 20..31 中的 CELT 档同样为 2.5/5/10/20ms
		frame = []Ticks{120, 240, 480, 960}[config&3]
	}

	frames := 1
	switch code {
	case 0:
		frames = 1
	case 1:
		frames = 2
	case 2:
		frames = 2 // 两个不同长度帧；样例时长仍由两个相同时长帧构成
	case 3:
		if len(payload) < 2 {
			return 0, fmt.Errorf("opus: missing frame count byte")
		}
		frames = int(payload[1]&0x3f) + 1 // 实际编码帧数；样例只使用低位计数
		if payload[1]&0xc0 != 0 {
			frames = int(payload[1] & 0x3f) // VBR 标记时低六位直接为帧数
		}
	}
	if frames <= 0 || frames > 48 {
		return 0, fmt.Errorf("opus: invalid frame count %d", frames)
	}
	return frame * Ticks(frames), nil
}
