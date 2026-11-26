package media

import (
	"fmt"
	"sort"
)

// FrameInput 单帧输入。
type FrameInput struct {
	Track string
	Seq   int64
	PTS   int64
	Dur   int64 // 刻度，0 表示缺失
	Hash  string
	File  string
	Bad   bool // 哈希校验失败，渲染时隔离
}

// Slot 时间线上的一个事件（帧或 GAP）。
type Slot struct {
	Track   string
	Seq     int64
	StartMs float64
	DurMs   float64
	Hash    string
	File    string
	Gap     bool
	Bad     bool
}

// UnwrapPTS 按模数与半周期规则展开回绕 PTS。
// 相邻差值经 ±modulus 调整后落入 [0, M/2) 视为合法；
// 恰为半周期无法消歧、或调整后仍倒退的段返回错误，禁止静默倒退。
// modulus 为 0 表示不回绕，仅检查单调性。
func UnwrapPTS(raw []int64, modulus int64) ([]int64, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if modulus < 0 {
		return nil, fmt.Errorf("media: negative modulus %d", modulus)
	}
	out := make([]int64, len(raw))
	out[0] = raw[0]
	for i := 1; i < len(raw); i++ {
		delta := raw[i] - raw[i-1]
		if modulus > 0 {
			half := modulus / 2
			if delta == half || delta == -half {
				return nil, fmt.Errorf("media: ambiguous pts jump %d at index %d (half period)", delta, i)
			}
			if delta < -half {
				delta += modulus
			} else if delta > half {
				delta -= modulus
			}
		}
		if delta < 0 {
			return nil, fmt.Errorf("media: pts moved backward at index %d (%d -> %d)", i, raw[i-1], raw[i])
		}
		out[i] = out[i-1] + delta
	}
	return out, nil
}

// BuildTrackTimeline 将单轨帧序列展开为时间线：
// 展开回绕、应用漂移校正、推导 VFR 帧时长、为缺失序号写入 GAP。
// defaultDurMs 为末帧及无法推导时的默认帧时长（通常 1000/fps）。
func BuildTrackTimeline(frames []FrameInput, tb Timebase, dc *DriftCorrection, modulus int64, defaultDurMs float64) ([]Slot, error) {
	if len(frames) == 0 {
		return nil, nil
	}
	if err := tb.Validate(); err != nil {
		return nil, err
	}
	fs := make([]FrameInput, len(frames))
	copy(fs, frames)
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].Seq < fs[j].Seq })
	raw := make([]int64, len(fs))
	for i, f := range fs {
		if i > 0 && f.Seq == fs[i-1].Seq {
			return nil, fmt.Errorf("media: duplicate seq %d in track %q", f.Seq, f.Track)
		}
		raw[i] = f.PTS
	}
	unwrapped, err := UnwrapPTS(raw, modulus)
	if err != nil {
		return nil, fmt.Errorf("track %q: %w", fs[0].Track, err)
	}
	starts := make([]float64, len(fs))
	for i, pts := range unwrapped {
		starts[i] = dc.CorrectMs(tb, pts)
	}
	var slots []Slot
	for i, f := range fs {
		// 到下一个有效帧之间均匀分摊缺失序号的时长
		gapCount := int64(0)
		if i+1 < len(fs) {
			gapCount = fs[i+1].Seq - f.Seq - 1
		}
		dur := frameDuration(fs, i, starts, tb, defaultDurMs, gapCount)
		if dur <= 0 {
			return nil, fmt.Errorf("track %q seq %d: non-positive duration", f.Track, f.Seq)
		}
		slots = append(slots, Slot{Track: f.Track, Seq: f.Seq, StartMs: starts[i],
			DurMs: dur, Hash: f.Hash, File: f.File, Bad: f.Bad})
		// 缺帧写 GAP，时间从上一帧结束起算，不挪动后续帧
		for g := int64(1); g <= gapCount; g++ {
			slots = append(slots, Slot{Track: f.Track, Seq: f.Seq + g,
				StartMs: starts[i] + dur*float64(g), DurMs: dur, Gap: true})
		}
	}
	return slots, nil
}

// frameDuration 优先采用帧自带时长；缺失时把到下一有效帧的间隔
// 在本帧与缺失序号间均分；末帧用默认时长。
func frameDuration(fs []FrameInput, i int, starts []float64, tb Timebase, defaultDurMs float64, gapCount int64) float64 {
	f := fs[i]
	if f.Dur > 0 {
		return tb.DurationMillis(f.Dur)
	}
	if i+1 < len(fs) {
		return (starts[i+1] - starts[i]) / float64(gapCount+1)
	}
	return defaultDurMs
}
