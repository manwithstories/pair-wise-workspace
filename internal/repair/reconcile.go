// Package repair 依据可信采样计数、扩展 RTP 序号与设备时钟
// 把重排后的采样块修复到统一采样时间线上。
// 任何不可修复区间都显式标注为 uncertain，绝不静默压缩时间线。
package repair

import (
	"pairflow/internal/clock"
	"pairflow/internal/rtp"
	"pairflow/internal/wav"
)

// Segment 是修复后落在统一时间线上的一个采样块。
type Segment struct {
	DeviceID     string
	StartSample  int64
	SampleCount  int64
	SourceOffset int64
	RTPSeq       uint64
	RTPTimestamp uint32
	Channels     uint16
	SampleRate   uint32
	Data         []byte
}

// Result 汇总单设备的修复结果。
type Result struct {
	DeviceID     string
	Timeline     *clock.Timeline
	Segments     []Segment
	Uncertain    []clock.UncertainRange
	BlockErrors  []*clock.StageError // 解析期可恢复错误（已继续处理）
	FormatErrors []*clock.StageError
	GapCount     int
}

// Reconciler 编排单设备的时间线修复。
type Reconciler struct {
	deviceID string
	tl       *clock.Timeline
	unw      clock.TSUnwrapper

	prev        *wav.Block
	lastSeq     uint64
	haveSeq     bool
	gapsApplied map[uint64]bool // 已落到时间线的缺口起始序号（去重）
	res         Result
}

// NewReconciler 创建设备级修复器。
func NewReconciler(deviceID string) *Reconciler {
	return &Reconciler{
		deviceID:    deviceID,
		gapsApplied: make(map[uint64]bool),
		res:         Result{DeviceID: deviceID},
	}
}

// HandleBlock 处理一个解析后的 WAV 块（含其携带的解析期错误）。
// rtpGaps 为重排器报告的全部突发缺口（按序号区间），内部按水位去重。
func (r *Reconciler) HandleBlock(blk *wav.Block, rtpGaps []rtp.Gap) *Result {
	// 透传解析期错误，损坏块不阻断后续块。
	r.res.BlockErrors = append(r.res.BlockErrors, blk.Errors...)

	// 扩展序号来自重排器；序号不连续处先消费 RTP 层缺口。
	if r.haveSeq && blk.RTPSeq > r.lastSeq+1 {
		r.consumeGaps(rtpGaps, r.lastSeq+1, blk.RTPSeq, blk.SourceOffset)
	}
	r.haveSeq = true
	r.lastSeq = blk.RTPSeq

	// 首块建立时间线与格式基准；格式变化显式记录但不终止。
	if r.tl == nil {
		if blk.Format != nil {
			r.tl = clock.NewTimeline(r.deviceID, blk.Format.SampleRate)
			r.res.Timeline = r.tl
		}
	} else if err := wav.DetectFormatChange(r.prev, blk); err != nil {
		r.res.FormatErrors = append(r.res.FormatErrors, err)
	}

	// 时间戳回绕展开与时间线放置。
	ts, tsWrapped := r.unw.Update(blk.RTPTimestamp)
	if tsWrapped && r.tl != nil {
		r.tl.NoteWrap()
	}
	if r.tl == nil || blk.Format == nil {
		// 连格式都无法确定的块：无法放置，记录为不确定区间后继续。
		if r.tl == nil {
			r.tl = clock.NewTimeline(r.deviceID, 0)
			r.res.Timeline = r.tl
		}
		ur := r.tl.MarkUncertain(r.tl.NextSample(), r.tl.NextSample()+blk.SampleCount(),
			clock.KindIndeterminate, "block without usable fmt chunk", blk.SourceOffset)
		r.res.Uncertain = append(r.res.Uncertain, ur)
		r.tl.InsertGap(blk.SampleCount(), clock.KindIndeterminate,
			"unparseable block retained on timeline", blk.SourceOffset)
		r.prev = blk
		return &r.res
	}

	pr := r.tl.Place(ts, blk.SampleCount(), blk.SourceOffset)
	if pr.Uncertain != nil {
		r.res.Uncertain = append(r.res.Uncertain, *pr.Uncertain)
	}
	if !blk.Recoverable {
		// 不可解析块以零数据段占位，区间保持不确定。
		ur := r.tl.MarkUncertain(pr.StartSample, pr.StartSample+blk.SampleCount(),
			clock.KindIndeterminate, "unrecoverable wav block", blk.SourceOffset)
		r.res.Uncertain = append(r.res.Uncertain, ur)
	}
	r.res.Segments = append(r.res.Segments, Segment{
		DeviceID: r.deviceID, StartSample: pr.StartSample,
		SampleCount: blk.SampleCount(), SourceOffset: blk.SourceOffset,
		RTPSeq: blk.RTPSeq, RTPTimestamp: blk.RTPTimestamp,
		Channels: blk.Format.Channels, SampleRate: blk.Format.SampleRate,
		Data: blk.Data,
	})
	r.prev = blk
	return &r.res
}

// consumeGaps 把重排器给出的序号缺口落到时间线上，
// 优先采用 RTP 层基于时间戳中位数的缺失采样估算。
func (r *Reconciler) consumeGaps(gaps []rtp.Gap, from, to uint64, offset int64) {
	for _, g := range gaps {
		if g.EndSeq <= from || g.StartSeq >= to || r.gapsApplied[g.StartSeq] {
			continue
		}
		r.gapsApplied[g.StartSeq] = true
		r.res.GapCount++
		missing := g.EstSamples
		if missing <= 0 {
			missing = int64(to - from)
		}
		if r.tl == nil {
			r.tl = clock.NewTimeline(r.deviceID, 0)
			r.res.Timeline = r.tl
		}
		ur := r.tl.InsertGap(missing, clock.KindBurstLoss,
			"burst rtp loss; samples missing and marked uncertain", offset)
		r.res.Uncertain = append(r.res.Uncertain, ur)
	}
}

// TailGaps 在输入结束时处理仍未闭合的尾部缺口。
func (r *Reconciler) TailGaps(gaps []rtp.Gap) *Result {
	if r.tl == nil {
		r.tl = clock.NewTimeline(r.deviceID, 0)
		r.res.Timeline = r.tl
	}
	for _, g := range gaps {
		if r.gapsApplied[g.StartSeq] {
			continue
		}
		r.gapsApplied[g.StartSeq] = true
		r.res.GapCount++
		missing := g.EstSamples
		if missing <= 0 {
			missing = int64(g.MissingPackets)
		}
		ur := r.tl.InsertGap(missing, clock.KindBurstLoss,
			"trailing burst rtp loss", g.NextGoodOffset)
		r.res.Uncertain = append(r.res.Uncertain, ur)
	}
	return &r.res
}

// Result 返回累计修复结果（含时间线与全部不确定区间）。
func (r *Reconciler) Result() *Result {
	if r.tl != nil {
		r.res.Timeline = r.tl
		r.res.Uncertain = r.tl.Uncertain
	}
	return &r.res
}

// HasIndeterminate 报告是否存在不可判定缺口（决定退出码 3）。
func (res *Result) HasIndeterminate() bool {
	for _, u := range res.Uncertain {
		if u.Kind == clock.KindIndeterminate {
			return true
		}
	}
	return false
}
