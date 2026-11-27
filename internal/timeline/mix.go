// Package timeline 把多机位音视频轨合成到统一时基，
// 统计端到端同步误差；缺帧或 PCM 缺块保留来源信息。
package timeline

import (
	"review/internal/clock"
	"review/internal/fault"
)

// Kind 轨道类型。
type Kind int

const (
	Video Kind = iota
	Audio
)

// Sample 统一时基上的一个样本。
type Sample struct {
	Tick int64 // 统一基准 tick
	Dur  int64 // 时长（tick）
	Ref  int   // 载荷引用（调用方自定义索引）
}

// Track 一条已映射到统一时基的轨道。
type Track struct {
	ID      string
	Kind    Kind
	Samples []Sample
}

// Gap 缺帧/缺块记录，保留来源。
type Gap struct {
	TrackID string
	Tick    int64
	Reason  string
}

// Slot 一个输出时间槽。
type Slot struct {
	Tick     int64
	Hits     map[string]int // trackID -> Sample.Ref
	SpreadMs float64        // 本槽各轨命中样本的最大时间偏差
}

// Timeline 合成结果。
type Timeline struct {
	Slots     []Slot
	Gaps      []Gap
	SyncMaxMs float64
	Base      clock.Base
}

// Mix 以 slotDur 为间隔合成轨道；每槽取各轨最近样本（容差半槽），
// 统计同步误差；缺样本记录带来源的 Gap。
func Mix(tracks []Track, slotDur int64, base clock.Base) *Timeline {
	tl := &Timeline{Base: base}
	if len(tracks) == 0 || slotDur <= 0 {
		return tl
	}
	var lo, hi int64
	for i, tr := range tracks {
		if len(tr.Samples) == 0 {
			continue
		}
		first, last := tr.Samples[0].Tick, tr.Samples[len(tr.Samples)-1].Tick
		if i == 0 || first < lo {
			lo = first
		}
		if i == 0 || last > hi {
			hi = last
		}
	}
	// 槽起点对齐到 slotDur 网格
	start := lo - (lo % slotDur)
	for tick := start; tick <= hi; tick += slotDur {
		slot := Slot{Tick: tick, Hits: map[string]int{}}
		var minT, maxT int64
		n := 0
		for _, tr := range tracks {
			s, ok := nearest(tr.Samples, tick, slotDur/2)
			if !ok {
				if tick >= lo {
					tl.Gaps = append(tl.Gaps, Gap{TrackID: tr.ID, Tick: tick,
						Reason: "missing sample in slot"})
				}
				continue
			}
			slot.Hits[tr.ID] = s.Ref
			if n == 0 || s.Tick < minT {
				minT = s.Tick
			}
			if n == 0 || s.Tick > maxT {
				maxT = s.Tick
			}
			n++
		}
		if n > 0 {
			slot.SpreadMs = base.Millis(maxT - minT)
			if slot.SpreadMs > tl.SyncMaxMs {
				tl.SyncMaxMs = slot.SpreadMs
			}
		}
		tl.Slots = append(tl.Slots, slot)
	}
	return tl
}

// nearest 二分查找距 tick 最近且误差不超过 tol 的样本。
func nearest(samples []Sample, tick, tol int64) (Sample, bool) {
	lo, hi := 0, len(samples)-1
	best := -1
	var bestDist int64
	for lo <= hi {
		mid := (lo + hi) / 2
		d := samples[mid].Tick - tick
		ad := d
		if ad < 0 {
			ad = -ad
		}
		if best < 0 || ad < bestDist {
			best, bestDist = mid, ad
		}
		if d < 0 {
			lo = mid + 1
		} else if d > 0 {
			hi = mid - 1
		} else {
			break
		}
	}
	if best < 0 || bestDist > tol {
		return Sample{}, false
	}
	return samples[best], true
}

// GapFaults 把缺口转换为可恢复故障（保留来源轨道与 tick）。
func (tl *Timeline) GapFaults() []*fault.Error {
	var errs []*fault.Error
	for _, g := range tl.Gaps {
		errs = append(errs, fault.Newf(fault.KindGap, g.TrackID, g.Tick, true, "%s", g.Reason))
	}
	return errs
}
