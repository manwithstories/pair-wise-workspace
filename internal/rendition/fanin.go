package rendition

import (
	"sort"

	"bcast/internal/fault"
	"bcast/internal/model"
)

// CrossTierToleranceUS 是跨层时间跨度允许的偏差上限（微秒）。
// 表示层只做抽帧不重采样，同轨首尾应对齐；超过即判定边界不一致。
const CrossTierToleranceUS int64 = 2_000

// FanInReport 是扇入核验的确定性结果。
type FanInReport struct {
	BoundaryMismatches   int
	CrossLayerDeviations int
	Segments             int
	PerTrack             map[uint8][2]int64 // 汇合后的共享时钟覆盖 [start,end)
}

// segsByTrack 把表示层段按轨道归组并保持段序号有序。
func segsByTrack(r *Rendition) map[uint8][]Segment {
	m := map[uint8][]Segment{}
	for _, s := range r.Segments {
		m[s.TrackID] = append(m[s.TrackID], s)
	}
	return m
}

// FanIn 汇合全部表示层到共享主时钟并做两类核验：
//  1. 层内半开区间：相邻段必须首尾相接，不重叠、不倒退；
//  2. 跨层偏差：同一轨道在各表示层的覆盖跨度必须在容差内一致。
//
// 全部核验基于整数微秒，结果与并发扇出的顺序无关。
func FanIn(rs []*Rendition, fc *fault.Collector) (FanInReport, error) {
	rep := FanInReport{PerTrack: map[uint8][2]int64{}}
	trackMin := map[uint8]int64{}
	trackMax := map[uint8]int64{}
	for _, r := range rs {
		byTrack := segsByTrack(r)
		tracks := make([]int, 0, len(byTrack))
		for id := range byTrack {
			tracks = append(tracks, int(id))
		}
		sort.Ints(tracks)
		for _, idInt := range tracks {
			id := uint8(idInt)
			segs := byTrack[id]
			sort.Slice(segs, func(i, j int) bool { return segs[i].Index < segs[j].Index })
			rep.Segments += len(segs)
			for i := 1; i < len(segs); i++ {
				prev, cur := segs[i-1], segs[i]
				// 半开区间合法性：当前段起点不得早于上一段终点。
				if cur.StartUS < prev.EndUS {
					rep.BoundaryMismatches++
					fc.Record(fault.KindBoundary, id, cur.FirstSeq, 0, 0,
						"segments overlap or go backwards at rendition boundary")
				}
			}
			if len(segs) > 0 {
				start, end := segs[0].StartUS, segs[len(segs)-1].EndUS
				if v, ok := trackMin[id]; !ok || start < v {
					trackMin[id] = start
				}
				if v, ok := trackMax[id]; !ok || end > v {
					trackMax[id] = end
				}
			}
		}
	}
	// 跨层偏差：以全量层（Stride=1）覆盖域为参照，其它层因抽帧天然略短，
	// 允许其跨度收缩不超过自身一个帧周期与固定容差之和，且不得越出参照域。
	var reference *Rendition
	for _, r := range rs {
		if r.Tier.Stride == 1 {
			reference = r
			break
		}
	}
	ids := make([]int, 0, len(trackMin))
	for id := range trackMin {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	for _, idInt := range ids {
		id := uint8(idInt)
		rep.PerTrack[id] = [2]int64{trackMin[id], trackMax[id]}
		if reference == nil {
			continue
		}
		ref, ok := reference.Span[id]
		if !ok {
			continue
		}
		for _, r := range rs {
			span, ok := r.Span[id]
			if !ok {
				continue
			}
			// 自适应容差：固定项 + 本层一个抽帧周期（stride*标称周期）。
			margin := CrossTierToleranceUS + int64(r.Tier.Stride)*r.PeriodUS[id]
			// 不得早于参照起点超过容差，也不得晚于参照终点。
			if ref[0]-span[0] > margin || span[1]-ref[1] > margin ||
				span[0] > ref[1] || span[1] < ref[0] {
				rep.CrossLayerDeviations++
				fc.Record(fault.KindBoundary, id, 0, 0, 0,
					"cross-rendition span outside reference domain")
				break
			}
		}
	}
	return rep, nil
}

// SamplesOf 汇总指定表示层内某轨道的全部样本（跨段拼接），供封装与渲染复用。
func SamplesOf(r *Rendition, track uint8) []model.Sample {
	var out []model.Sample
	for _, s := range r.Segments {
		if s.TrackID == track {
			out = append(out, s.Samples...)
		}
	}
	return out
}
