// Package metrics 汇总管线各阶段的延迟、音画同步误差、恢复率与资源占用，
// 产出演示入口要求的单行 JSON 报告。
package metrics

import (
	"sort"

	"mediad/internal/model"
)

// Report 对应终端输出的单行 JSON 字段。
type Report struct {
	SyncErrorMS    float64              `json:"sync_error_ms"`
	RecoveryRate   float64              `json:"recovery_rate"`
	P95LatencyMS   float64              `json:"p95_latency_ms"`
	KeyframeGapMS  uint64               `json:"keyframe_gap_ms"`
	CorruptRanges  []model.CorruptRange `json:"corrupt_ranges"`
	QueuePeak      int                  `json:"queue_peak"`
	BackpressureMS int64                `json:"backpressure_ms"`
}

// Aggregator 累积原始观测并在 Snapshot 时计算分位数与比率。
type Aggregator struct {
	latenciesMS []float64
	syncErrsMS  []float64
}

// NewAggregator 构造聚合器。
func NewAggregator() *Aggregator { return &Aggregator{} }

// ObserveLatency 记录一个块的端到端延迟（毫秒）。
func (a *Aggregator) ObserveLatency(ms float64) {
	if ms < 0 {
		ms = 0
	}
	a.latenciesMS = append(a.latenciesMS, ms)
}

// ObserveSyncError 记录一次音画同步误差的绝对值（毫秒）。
func (a *Aggregator) ObserveSyncError(ms float64) {
	if ms < 0 {
		ms = -ms
	}
	a.syncErrsMS = append(a.syncErrsMS, ms)
}

// percentile 用最近秩法计算 p 分位数（p∈[0,100]）。
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	// 最近秩：ceil(p/100 * N)，1 基索引。
	rank := int((p/100)*float64(len(sorted)) + 0.999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// Snapshot 结合恢复率、关键帧间隔、队列与损坏区段生成最终报告。
func (a *Aggregator) Snapshot(
	recoveryRate float64,
	keyframeGapMS uint64,
	queuePeak int,
	backpressureMS int64,
	corrupt []model.CorruptRange,
) Report {
	lat := append([]float64(nil), a.latenciesMS...)
	sort.Float64s(lat)
	sync := append([]float64(nil), a.syncErrsMS...)
	sort.Float64s(sync)
	if corrupt == nil {
		corrupt = []model.CorruptRange{}
	}
	return Report{
		SyncErrorMS:    mean(sync),
		RecoveryRate:   recoveryRate,
		P95LatencyMS:   percentile(lat, 95),
		KeyframeGapMS:  keyframeGapMS,
		CorruptRanges:  corrupt,
		QueuePeak:      queuePeak,
		BackpressureMS: backpressureMS,
	}
}

// MaxSyncError 返回观测到的最大同步误差，供阈值判定。
func (a *Aggregator) MaxSyncError() float64 {
	if len(a.syncErrsMS) == 0 {
		return 0
	}
	m := a.syncErrsMS[0]
	for _, x := range a.syncErrsMS[1:] {
		if x > m {
			m = x
		}
	}
	return m
}
