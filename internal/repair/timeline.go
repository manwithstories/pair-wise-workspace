// Package repair 把解析出的包事件恢复成统一时间轴并切分片段。
package repair

import (
	"fmt"

	"mediatool/internal/clock"
	"mediatool/internal/ts"
)

// Frame 是时间轴上的一帧。
type Frame struct {
	PTS       clock.Ticks // 展开回绕后的绝对刻度
	Recovered bool        // 由连续计数缺口恢复插入
}

// Segment 是固定时长的输出片段。
type Segment struct {
	ID       int
	Start    clock.Ticks
	End      clock.Ticks
	Complete bool
	Faults   clock.Faults
}

// Timeline 是恢复后的表示层时间轴。
type Timeline struct {
	Frames   []Frame
	Segments []Segment
	Faults   clock.Faults
	Origin   clock.Ticks
}

// PESPTS 从 PES 负载头解析 33 位 PTS。
func PESPTS(payload []byte) (uint64, bool) {
	if len(payload) < 14 || payload[0] != 0 || payload[1] != 0 || payload[2] != 1 {
		return 0, false
	}
	if payload[7]&0x80 == 0 {
		return 0, false
	}
	p := payload[9:14]
	pts := uint64(p[0]>>1&0x07)<<30 | uint64(p[1])<<22 |
		uint64(p[2]>>1&0x7F)<<15 | uint64(p[3])<<7 | uint64(p[4]>>1&0x7F)
	return pts, true
}

// Recover 从 TS 解析结果重建视频时间轴：
// 展开 PCR/PTS 回绕，按连续计数缺口插入恢复帧，
// 再按 segmentMs 切分片段；未恢复故障会使所在片段标记为不完整。
// extra 用于汇入 PSI、音频等其它子系统的故障。
func Recover(res ts.ParseResult, videoPID uint16, segmentMs int, extra clock.Faults) Timeline {
	tl := Timeline{}
	var ptsU, pcrU clock.Unwrapper
	wrapSeen := 0
	// pktPTS 记录每个视频包偏移对应的展开 PTS，用于把故障映射到片段。
	pktPTS := map[int64]clock.Ticks{}
	for _, p := range res.Packets {
		if p.HasPCR {
			pcrU.Unwrap(uint64(p.PCRBase))
			if pcrU.WrapCount() > wrapSeen {
				wrapSeen = pcrU.WrapCount()
				tl.Faults = append(tl.Faults, clock.Fault{
					Kind: clock.FaultPCRWrap, Offset: p.Offset, PID: int32(p.PID),
					Recovered: true, Detail: "PCR wrapped past 2^33; timeline unwrapped"})
			}
		}
		if p.PID == videoPID && p.PayloadStart {
			if raw, ok := PESPTS(p.Payload); ok {
				pts := clock.Ticks(ptsU.Unwrap(raw))
				tl.Frames = append(tl.Frames, Frame{PTS: pts})
				pktPTS[p.Offset] = pts
			}
		}
	}
	if len(tl.Frames) == 0 {
		return tl
	}
	delta := clock.Ticks(3600)
	if len(tl.Frames) >= 2 && tl.Frames[1].PTS > tl.Frames[0].PTS {
		delta = tl.Frames[1].PTS - tl.Frames[0].PTS
	}
	// 恢复可修复的连续计数缺口：在缺口位置插入恢复帧。
	inserts := 0
	for _, f := range res.Faults {
		if f.Kind != clock.FaultCCGap || int32(f.PID) != int32(videoPID) {
			continue
		}
		idx := len(tl.Frames)
		for i, fr := range tl.Frames {
			if pktPTS[f.Offset] <= fr.PTS {
				idx = i
				break
			}
		}
		if idx == 0 {
			continue
		}
		rec := Frame{PTS: tl.Frames[idx-1].PTS + delta, Recovered: true}
		tl.Frames = append(tl.Frames, Frame{})
		copy(tl.Frames[idx+1:], tl.Frames[idx:])
		tl.Frames[idx] = rec
		inserts++
	}
	_ = inserts
	tl.Origin = tl.Frames[0].PTS
	segTicks := clock.MsToTicks(int64(segmentMs))
	last := tl.Frames[len(tl.Frames)-1].PTS
	nSeg := int((last-tl.Origin)/segTicks) + 1
	tl.Segments = make([]Segment, nSeg)
	for i := range tl.Segments {
		tl.Segments[i] = Segment{
			ID: i, Start: tl.Origin + clock.Ticks(i)*segTicks,
			End: tl.Origin + clock.Ticks(i+1)*segTicks, Complete: true,
		}
	}
	// 汇总故障：未恢复的映射到所在片段并标记不完整。
	all := append(append(clock.Faults{}, res.Faults...), extra...)
	segOf := func(off int64) int {
		best := nSeg - 1
		var bestPTS clock.Ticks = -1
		for o, pts := range pktPTS {
			if o <= off && pts > bestPTS {
				bestPTS = pts
			}
		}
		if bestPTS >= 0 {
			if id := int((bestPTS - tl.Origin) / segTicks); id < nSeg {
				best = id
			}
		}
		return best
	}
	for _, f := range all {
		tl.Faults = append(tl.Faults, f)
		if f.Recovered {
			continue
		}
		id := segOf(f.Offset)
		tl.Segments[id].Complete = false
		tl.Segments[id].Faults = append(tl.Segments[id].Faults, f)
	}
	return tl
}

// String 输出时间轴摘要（用于终端展示）。
func (tl Timeline) String() string {
	incomplete := 0
	for _, s := range tl.Segments {
		if !s.Complete {
			incomplete++
		}
	}
	return fmt.Sprintf("frames=%d segments=%d incomplete=%d faults=%d",
		len(tl.Frames), len(tl.Segments), incomplete, len(tl.Faults))
}
