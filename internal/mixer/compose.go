// Package mixer 把多条轨道的媒体块按统一 27 MHz 时间轴对齐，
// 保留可变帧率节奏，产出供渲染层消费的合成帧计划。
package mixer

import (
	"sort"

	"tsproof/internal/adapter"
	"tsproof/internal/clock"
)

// Slot 是共同时间轴上的一个对齐槽位：一个视频帧及其时间重叠的音频块。
type Slot struct {
	At        clock.Tick       // 槽位时刻（取视频帧 PTS）
	Frame     *adapter.Block   // 视频帧（必有）
	Audio     []*adapter.Block // 落在本帧时间窗内的音频块
	FrameKind clock.FrameKind
	Untrusted bool
}

// Plan 是整段流的对齐结果与统计。
type Plan struct {
	Slots       []Slot
	VideoTrack  uint16
	AudioTrack  uint16
	OrphanAudio int // 早于首帧或晚于末帧、无法挂到任何槽位的音频块
}

// Compose 按视频帧时刻切槽，把音频块归入时间上重叠的槽位。
//
// 视频帧严格按其 PTS 排列（VFR 间隔不变），音频块用双指针就近归槽，
// 保证 O(n+m) 且输出确定性。无视频帧时返回空计划。
func Compose(blocks map[uint16][]*adapter.Block, videoPID, audioPID uint16) *Plan {
	plan := &Plan{VideoTrack: videoPID, AudioTrack: audioPID}
	frames := blocks[videoPID]
	if len(frames) == 0 {
		plan.OrphanAudio = len(blocks[audioPID])
		return plan
	}
	// 帧按 PTS 升序（适配器已保证索引单调，这里再防御性排序）。
	sortedFrames := make([]*adapter.Block, len(frames))
	copy(sortedFrames, frames)
	sort.SliceStable(sortedFrames, func(i, j int) bool {
		return sortedFrames[i].PTS27 < sortedFrames[j].PTS27
	})

	audio := blocks[audioPID]
	slotEnd := func(i int) clock.Tick {
		if i+1 < len(sortedFrames) {
			return clock.Tick(sortedFrames[i+1].PTS27)
		}
		return clock.Tick(sortedFrames[i].PTS27) + clock.TicksPerSecond // 末帧给 1s 窗口
	}

	ai := 0
	for fi, frame := range sortedFrames {
		start := clock.Tick(frame.PTS27)
		end := slotEnd(fi)
		slot := Slot{
			At:        start,
			Frame:     frame,
			FrameKind: kindOf(frame.Key),
			Untrusted: frame.Untrusted,
		}
		// 推进音频指针到当前窗口起点；落后的音频记为孤儿。
		for ai < len(audio) && clock.Tick(audio[ai].PTS27) < start {
			plan.OrphanAudio++
			ai++
		}
		for ai < len(audio) && clock.Tick(audio[ai].PTS27) < end {
			slot.Audio = append(slot.Audio, audio[ai])
			if audio[ai].Untrusted {
				slot.Untrusted = true
			}
			ai++
		}
		plan.Slots = append(plan.Slots, slot)
	}
	plan.OrphanAudio += len(audio) - ai
	return plan
}

func kindOf(key bool) clock.FrameKind {
	if key {
		return clock.FrameKey
	}
	return clock.FrameInter
}

// SlotTimes 返回全部槽位时刻，供渲染层与清单核对。
func (p *Plan) SlotTimes() []clock.Tick {
	out := make([]clock.Tick, len(p.Slots))
	for i, s := range p.Slots {
		out[i] = s.At
	}
	return out
}
