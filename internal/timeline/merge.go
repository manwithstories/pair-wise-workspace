// Package timeline 把探针包流合并为统一主时基（微秒）下的多轨时间线。
// 职责边界：保留原始 PTS 的回绕展开与整数归一化、按 (轨道,序号,PTS) 去重、
// 把丢包缺口标注到归一化时间轴。任何换算溢出都作为错误上抛，绝不静默截断。
package timeline

import (
	"sort"

	"bcast/internal/clock"
	"bcast/internal/fault"
	"bcast/internal/model"
)

// PacketSource 是时间线对输入侧的最小依赖，*probe.Reader 天然满足。
type PacketSource interface {
	ReadPacket() (model.Packet, bool, error)
	Plan() model.Plan
	Gaps() []model.Gap
}

// Timeline 是合并产物：全局有序样本、按轨样本与带时间标注的缺口。
type Timeline struct {
	Samples []model.Sample
	ByTrack map[uint8][]model.Sample
	Gaps    []model.Gap
	Plan    model.Plan
}

type trackState struct {
	unwrap   *clock.Unwrapper
	timebase uint64
	seen     map[[2]uint64]bool // 已见 (序号, 原始 PTS)，按规范复合键去重
}

// Build 消费整个包流并产出确定性时间线。重复包去重并记录故障；
// 回绕由各轨独立 Unwrapper 展开；缺口时间范围由相邻样本归一化 PTS 界定。
func Build(src PacketSource, fc *fault.Collector) (*Timeline, error) {
	plan := src.Plan()
	states := make(map[uint8]*trackState, len(plan.Tracks))
	for _, t := range plan.Tracks {
		u, err := clock.NewUnwrapper(plan.WrapModulus[t.ID])
		if err != nil {
			return nil, fault.New(fault.ErrInvalidInput, fault.KindWrap, t.ID, 0, 0, 0, err.Error())
		}
		states[t.ID] = &trackState{unwrap: u, timebase: t.Timebase, seen: map[[2]uint64]bool{}}
	}
	tl := &Timeline{ByTrack: map[uint8][]model.Sample{}, Plan: plan}
	for {
		pkt, ok, err := src.ReadPacket()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		st, known := states[pkt.TrackID]
		if !known {
			return nil, fault.InvalidInputf("packet for undeclared track %d", pkt.TrackID)
		}
		// 去重键：轨道 + 序号 + 原始 PTS，三者一致即判定重复包。
		key := [2]uint64{pkt.Seq, pkt.PTS}
		if st.seen[key] {
			fc.Record(fault.KindDupe, pkt.TrackID, pkt.Seq, pkt.ByteStart, pkt.ByteEnd,
				"duplicate packet removed by timeline")
			continue
		}
		st.seen[key] = true
		raw, err := st.unwrap.Unwrap(pkt.PTS)
		if err != nil {
			return nil, fault.New(fault.ErrBoundary, fault.KindWrap, pkt.TrackID, pkt.Seq,
				pkt.ByteStart, pkt.ByteEnd, err.Error())
		}
		ptsUS, err := clock.ConvertU(raw, st.timebase, clock.MasterTimebase)
		if err != nil {
			return nil, fault.New(err, fault.KindWrap, pkt.TrackID, pkt.Seq,
				pkt.ByteStart, pkt.ByteEnd, "pts normalization overflow")
		}
		durUS, err := clock.ConvertU(uint64(pkt.Duration), st.timebase, clock.MasterTimebase)
		if err != nil {
			return nil, fault.New(err, fault.KindWrap, pkt.TrackID, pkt.Seq,
				pkt.ByteStart, pkt.ByteEnd, "duration normalization overflow")
		}
		if ptsUS > uint64(maxInt64) || durUS > uint64(maxInt64) {
			return nil, fault.New(clock.ErrOverflow, fault.KindWrap, pkt.TrackID, pkt.Seq,
				pkt.ByteStart, pkt.ByteEnd, "normalized value exceeds int64")
		}
		// 合并后帧体不再参与时间线/渲染（渲染由元数据确定性合成），
		// 置空 Payload 以约束内存，避免 30 分钟素材整体驻留。
		s := model.Sample{
			TrackID: pkt.TrackID, Kind: pkt.Kind, Seq: pkt.Seq,
			PTS: int64(ptsUS), Duration: int64(durUS), Wrap: st.unwrap.Wraps(),
		}
		tl.ByTrack[pkt.TrackID] = append(tl.ByTrack[pkt.TrackID], s)
		tl.Samples = append(tl.Samples, s)
	}
	// 全局确定性排序：PTS 优先，其次轨道、序号，保证同输入同序。
	sort.Slice(tl.Samples, func(i, j int) bool {
		a, b := tl.Samples[i], tl.Samples[j]
		if a.PTS != b.PTS {
			return a.PTS < b.PTS
		}
		if a.TrackID != b.TrackID {
			return a.TrackID < b.TrackID
		}
		return a.Seq < b.Seq
	})
	tl.resolveGaps(src.Gaps())
	return tl, nil
}

const maxInt64 = int64(^uint64(0) >> 1)

// resolveGaps 用相邻样本的归一化 PTS 为探针发现的序号缺口填充时间范围。
// 缺口在时间线上保持为半开空洞 [StartPTS, EndPTS)，不做插值填补。
func (tl *Timeline) resolveGaps(rawGaps []model.Gap) {
	index := map[[2]uint64]model.Sample{} // (track, seq) -> sample
	for _, s := range tl.Samples {
		index[[2]uint64{uint64(s.TrackID), s.Seq}] = s
	}
	for _, g := range rawGaps {
		if before, ok := index[[2]uint64{uint64(g.TrackID), g.BeforeSeq}]; ok {
			g.StartPTS = before.PTS
			g.EndPTS = before.PTS
			if after, ok2 := index[[2]uint64{uint64(g.TrackID), g.AfterSeq}]; ok2 {
				g.StartPTS = after.PTS + after.Duration
			}
			g.EndPTS = before.PTS
		}
		tl.Gaps = append(tl.Gaps, g)
	}
	sort.Slice(tl.Gaps, func(i, j int) bool {
		if tl.Gaps[i].TrackID != tl.Gaps[j].TrackID {
			return tl.Gaps[i].TrackID < tl.Gaps[j].TrackID
		}
		return tl.Gaps[i].AfterSeq < tl.Gaps[j].AfterSeq
	})
}

// Track 返回指定轨道的有序样本（不存在返回 nil）。
func (tl *Timeline) Track(id uint8) []model.Sample { return tl.ByTrack[id] }
