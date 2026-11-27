// Package timeline 实现恢复流水线的时间线：迟到窗口估计、单调水位推进、
// 突发丢失占位与检查点回溯重算。
package timeline

import (
	"archivemedia/internal/media"
	"archivemedia/internal/packet"
)

// minLateWindow 与 maxLateWindow 限定迟到窗口（微秒）。
// 窗口随带宽估计自适应，但永远落在该闭区间内。
const (
	minLateWindowMicros = 20_000
	maxLateWindowMicros = 5_000_000
)

// BandwidthEstimator 基于已到达包的到达间隔做 EWMA 平滑，
// 输出迟到窗口建议值。全部输入为确定性时间戳，无墙钟依赖。
type BandwidthEstimator struct {
	ewmaGapMicros float64
	lastArrival   int64
	have          bool
	// alpha 是 EWMA 平滑系数，固定取值保证可复现。
	alpha float64
}

// NewBandwidthEstimator 构造估计器。
func NewBandwidthEstimator() *BandwidthEstimator {
	return &BandwidthEstimator{alpha: 0.25}
}

// Observe 输入相邻包的到达时间（微秒）。首个样本只初始化基线。
func (b *BandwidthEstimator) Observe(arrivalMicros int64) {
	if !b.have {
		b.have = true
		b.ewmaGapMicros = 0
		b.lastArrival = arrivalMicros
		return
	}
	gap := arrivalMicros - b.lastArrival
	if gap < 0 {
		gap = 0
	}
	if b.ewmaGapMicros == 0 {
		b.ewmaGapMicros = float64(gap)
	} else {
		b.ewmaGapMicros = b.alpha*float64(gap) + (1-b.alpha)*b.ewmaGapMicros
	}
	b.lastArrival = arrivalMicros
}

// LateWindowMicros 返回当前迟到窗口建议值（微秒）。
// 带宽越紧（到达间隔越大），窗口越宽，给重传留更多时间。
func (b *BandwidthEstimator) LateWindowMicros() int64 {
	if !b.have || b.ewmaGapMicros <= 0 {
		return minLateWindowMicros
	}
	// 取间隔的 8 倍作为容忍度，再夹到闭区间。
	w := int64(b.ewmaGapMicros * 8)
	if w < minLateWindowMicros {
		w = minLateWindowMicros
	}
	if w > maxLateWindowMicros {
		w = maxLateWindowMicros
	}
	return w
}

// Watermark 是单调推进的时间线水位。
//
// 水位以到达时间（微秒）为推进坐标：所有到达时间 <= 水位的包都已被
// 时间线吸收，水位之后到达的包属于迟到包，必须触发回溯重算。
type Watermark struct {
	watermarkMicros int64
	advanced        bool
	lateWindow      *BandwidthEstimator
}

// NewWatermark 构造绑定带宽估计器的水位。
func NewWatermark(bwe *BandwidthEstimator) *Watermark {
	if bwe == nil {
		bwe = NewBandwidthEstimator()
	}
	return &Watermark{lateWindow: bwe}
}

// Advance 尝试把水位推进到 arrivalMicros。水位只进不退：
// 小于等于当前水位的推进请求被忽略。返回实际水位。
func (w *Watermark) Advance(arrivalMicros int64) int64 {
	if !w.advanced || arrivalMicros > w.watermarkMicros {
		w.watermarkMicros = arrivalMicros
		w.advanced = true
	}
	return w.watermarkMicros
}

// Position 返回当前水位（微秒）；未推进过时返回 0,false。
func (w *Watermark) Position() (int64, bool) {
	return w.watermarkMicros, w.advanced
}

// IsLate 报告事件是否落在水位之后（迟到）。
func (w *Watermark) IsLate(e *packet.Event) bool {
	if !w.advanced {
		return false
	}
	return e.ArrivalMicros > w.watermarkMicros
}

// WithinLateWindow 判断迟到事件是否仍在容忍窗口内；
// 超出窗口的迟到包按不可恢复损坏上报。
func (w *Watermark) WithinLateWindow(e *packet.Event) bool {
	if !w.advanced {
		return true
	}
	overdue := e.ArrivalMicros - w.watermarkMicros
	return overdue <= w.lateWindow.LateWindowMicros()
}

// LateWindow 暴露底层带宽估计器，供流水线喂入到达样本。
func (w *Watermark) LateWindow() *BandwidthEstimator { return w.lateWindow }

// Reset 在回溯重算后把水位回退到检查点位置。
// 这是唯一允许水位后退的入口，仅由 Replayer 调用。
func (w *Watermark) Reset(checkpointMicros int64) {
	w.watermarkMicros = checkpointMicros
	w.advanced = true
}

// GuardLate 是迟到包的统一门禁，返回包装后的媒体错误类别。
func (w *Watermark) GuardLate(e *packet.Event) error {
	if !w.IsLate(e) {
		return nil
	}
	if !w.WithinLateWindow(e) {
		return media.E(media.KindUnrecoverable, "timeline.guard-late",
			e.Location(), "迟到包超出最大容忍窗口", nil)
	}
	return media.E(media.KindTimeline, "timeline.guard-late",
		e.Location(), "水位后迟到包，需要回溯重算", nil)
}
