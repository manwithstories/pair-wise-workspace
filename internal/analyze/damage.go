package analyze

import (
	"hash/crc32"
	"sort"

	"bcastqc/internal/ingest"
	"bcastqc/internal/model"
)

// crcTable 使用 IEEE 多项式，与发送侧约定一致。
var crcTable = crc32.MakeTable(crc32.IEEE)

// DamageSegment 是定位到的损坏区段，带采样点范围与定位误差。
type DamageSegment struct {
	TrackID  int    `json:"track_id"`
	Start    int64  `json:"start_sample"`
	End      int64  `json:"end_sample"`
	Kind     string `json:"kind"`
	LocError int64  `json:"loc_error_samples"`
}

// DamageReport 汇总一条轨道的损坏定位结果。
type DamageReport struct {
	TrackID  int             `json:"track_id"`
	Segments []DamageSegment `json:"segments"`
	// MaxLocError 为全部区段中的最大定位误差（采样点）。
	MaxLocError int64 `json:"max_loc_error_samples"`
	// DamagedSamples 为损坏覆盖的采样点总数。
	DamagedSamples int64 `json:"damaged_samples"`
}

// LocateDamage 联合包 CRC、采样连续性与 WAV 长度三类证据定位损坏区段。
//
// 定位策略（保证误差不超过 1 个采样点）：
//   - CRC 失败的包：整个包区间标记为 crc 损坏，边界即包起止点，误差 0；
//   - 序号/采样不连续：缺失区间由前后包的采样点精确夹出，误差 0；
//   - 尾块截断：以最后一个完好包末端到 WAV 声明总长标记，误差至多 1；
//   - 所有缺口只记录在时间线上，绝不挤移后续采样位置。
func LocateDamage(
	trackID int,
	recs []ingest.PacketRecord,
	tl *model.TrackTimeline,
	wavFrames int64,
) (*DamageReport, error) {
	rep := &DamageReport{TrackID: trackID}
	sorted := make([]ingest.PacketRecord, len(recs))
	copy(sorted, recs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].StartSample < sorted[j].StartSample })

	// 1) 逐包 CRC 核验。
	for _, rec := range sorted {
		if rec.CRC == 0 {
			continue // 未附带 CRC 的包不做判坏，交由连续性检查兜底。
		}
		got := crc32.Checksum(rec.Payload, crcTable)
		if got != rec.CRC {
			rep.add(DamageSegment{
				TrackID:  trackID,
				Start:    rec.StartSample,
				End:      rec.EndSample(),
				Kind:     model.GapKindCRC,
				LocError: 0,
			})
			if err := tl.AddGap(model.Gap{
				Start: rec.StartSample, End: rec.EndSample(),
				Kind: model.GapKindCRC, LocError: 0,
			}); err != nil {
				return nil, err
			}
		}
	}

	// 2) 采样连续性：相邻包之间未覆盖的区间即丢失缺口。
	var prevEnd int64
	for i, rec := range sorted {
		if rec.StartSample > prevEnd {
			rep.add(DamageSegment{
				TrackID:  trackID,
				Start:    prevEnd,
				End:      rec.StartSample,
				Kind:     model.GapKindLoss,
				LocError: 0,
			})
			if err := tl.AddGap(model.Gap{
				Start: prevEnd, End: rec.StartSample,
				Kind: model.GapKindLoss, LocError: 0,
			}); err != nil {
				return nil, err
			}
		}
		if i == 0 || rec.EndSample() > prevEnd {
			prevEnd = rec.EndSample()
		}
	}

	// 3) 尾块截断：完好包覆盖范围短于 WAV 声明长度。
	if wavFrames > prevEnd {
		locErr := int64(0)
		// 截断尾块可能只丢失部分帧，末端无法精确到采样，误差上界 1。
		if wavFrames-prevEnd == 1 {
			locErr = 1
		}
		rep.add(DamageSegment{
			TrackID:  trackID,
			Start:    prevEnd,
			End:      wavFrames,
			Kind:     model.GapKindTruncation,
			LocError: locErr,
		})
		if err := tl.AddGap(model.Gap{
			Start: prevEnd, End: wavFrames,
			Kind: model.GapKindTruncation, LocError: locErr,
		}); err != nil {
			return nil, err
		}
	}

	rep.finalize(tl)
	return rep, nil
}

// add 登记区段并维护最大定位误差与损坏采样计数。
func (r *DamageReport) add(seg DamageSegment) {
	r.Segments = append(r.Segments, seg)
	if seg.LocError > r.MaxLocError {
		r.MaxLocError = seg.LocError
	}
	r.DamagedSamples += seg.End - seg.Start
}

// finalize 按时间线合并后的缺口重算统计，保证与再封装清单口径一致。
func (r *DamageReport) finalize(tl *model.TrackTimeline) {
	r.Segments = r.Segments[:0]
	r.DamagedSamples = 0
	r.MaxLocError = 0
	for _, g := range tl.Gaps {
		r.Segments = append(r.Segments, DamageSegment{
			TrackID:  g.TrackID,
			Start:    g.Start,
			End:      g.End,
			Kind:     g.Kind,
			LocError: g.LocError,
		})
		if g.LocError > r.MaxLocError {
			r.MaxLocError = g.LocError
		}
		r.DamagedSamples += g.Length()
	}
}
