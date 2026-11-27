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

// DamageTracker 以流式方式逐包定位损坏，内存只与当前连续性游标有关，
// 不缓存整段素材。缺口即时登记到时间线，绝不挤移后续采样位置。
type DamageTracker struct {
	trackID   int
	timeline  *model.TrackTimeline
	wavFrames int64
	// prevEnd 为已确认完好覆盖到的采样位置。
	prevEnd int64
	started bool
	report  *DamageReport
}

// NewDamageTracker 创建流式损坏定位器。
func NewDamageTracker(trackID int, tl *model.TrackTimeline, wavFrames int64) *DamageTracker {
	return &DamageTracker{
		trackID:   trackID,
		timeline:  tl,
		wavFrames: wavFrames,
		report:    &DamageReport{TrackID: trackID},
	}
}

// Observe 处理一个按序释放的包记录，即时识别 CRC 损坏与采样不连续。
func (d *DamageTracker) Observe(rec ingest.PacketRecord) error {
	// 1) CRC 核验：失败则整包区间标记为 crc 损坏，边界即包起止点，误差 0。
	crcBad := rec.CRC != 0 && crc32.Checksum(rec.Payload, crcTable) != rec.CRC
	if crcBad {
		if err := d.addGap(model.Gap{
			Start: rec.StartSample, End: rec.EndSample(),
			Kind: model.GapKindCRC, LocError: 0,
		}); err != nil {
			return err
		}
	}

	// 2) 采样连续性：与已确认覆盖位置之间未覆盖的区间即丢失缺口。
	// 时间线从采样 0 起算，因此首个幸存包若不从 0 开始，其前方整段也是丢失。
	if rec.StartSample > d.prevEnd {
		if err := d.addGap(model.Gap{
			Start: d.prevEnd, End: rec.StartSample,
			Kind: model.GapKindLoss, LocError: 0,
		}); err != nil {
			return err
		}
	}
	d.started = true
	// 无论 CRC 是否失败，包在时间线上都占据其采样区间（位置存在、内容损坏），
	// 因此连续性游标照常推进，避免把 CRC 坏区误判成采样丢失。
	if rec.EndSample() > d.prevEnd {
		d.prevEnd = rec.EndSample()
	}
	return nil
}

// Finish 在包流结束后补记尾块截断并产出最终报告。
func (d *DamageTracker) Finish() *DamageReport {
	// 3) 尾块截断：完好覆盖范围短于 WAV 声明长度。
	if d.wavFrames > d.prevEnd {
		locErr := int64(0)
		// 截断尾块可能只丢失部分帧，末端无法精确到采样，误差上界 1。
		if d.wavFrames-d.prevEnd == 1 {
			locErr = 1
		}
		_ = d.addGap(model.Gap{
			Start: d.prevEnd, End: d.wavFrames,
			Kind: model.GapKindTruncation, LocError: locErr,
		})
	}
	// 以时间线合并后的缺口为准重算统计，保证与再封装清单口径一致。
	d.report.Segments = d.report.Segments[:0]
	d.report.DamagedSamples = 0
	d.report.MaxLocError = 0
	for _, g := range d.timeline.Gaps {
		d.report.Segments = append(d.report.Segments, DamageSegment{
			TrackID: g.TrackID, Start: g.Start, End: g.End,
			Kind: g.Kind, LocError: g.LocError,
		})
		if g.LocError > d.report.MaxLocError {
			d.report.MaxLocError = g.LocError
		}
		d.report.DamagedSamples += g.Length()
	}
	return d.report
}

// addGap 登记缺口到时间线并同步原始区段统计（最终以 Finish 合并口径为准）。
func (d *DamageTracker) addGap(g model.Gap) error {
	g.TrackID = d.trackID
	return d.timeline.AddGap(g)
}

// LocateDamage 保留一次性全量入口，便于测试与离线场景复用：
// 内部仍走流式定位器，按起始采样点排序后逐条喂入。
func LocateDamage(
	trackID int,
	recs []ingest.PacketRecord,
	tl *model.TrackTimeline,
	wavFrames int64,
) (*DamageReport, error) {
	sorted := make([]ingest.PacketRecord, len(recs))
	copy(sorted, recs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].StartSample < sorted[j].StartSample })
	tr := NewDamageTracker(trackID, tl, wavFrames)
	for _, rec := range sorted {
		if err := tr.Observe(rec); err != nil {
			return nil, err
		}
	}
	return tr.Finish(), nil
}
