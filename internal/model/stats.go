package model

import "time"

// LatencySample 是一帧端到端处理时延的采样（纳秒）。
type LatencySample struct {
	Seq    uint32
	LatNS  int64
}

// Stats 汇总验收输出的全部量化指标。
type Stats struct {
	SyncErrorNS       int64 // 重建媒体时间相对音频锚点的最大残差
	LossInjected      int   // 注入的传输故障包数
	LossRecovered     int   // 经序号窗口恢复的包数
	RecoveryRate      float64
	P95LatencyNS      int64
	Segments          int
	BoundaryMismatch  int
	ManifestSHA256    string
	UnrecoverableGaps []Gap
}

// P95Latency 以最近窗口内的采样计算 P95，内存随窗口大小有界。
// 采样按到达顺序追加；使用对窗口副本排序，不改变原序列。
func P95Latency(samples []LatencySample) int64 {
	if len(samples) == 0 {
		return 0
	}
	vals := make([]int64, len(samples))
	for i, s := range samples {
		vals[i] = s.LatNS
	}
	// 插入排序对有界窗口足够，避免引入 sort 之外的分配热点。
	for i := 1; i < len(vals); i++ {
		for j := i; j > 0 && vals[j-1] > vals[j]; j-- {
			vals[j-1], vals[j] = vals[j], vals[j-1]
		}
	}
	idx := (len(vals)*95 + 99) / 100 // 向上取整的 95 分位序号
	if idx >= len(vals) {
		idx = len(vals) - 1
	}
	return vals[idx]
}

// SinceNS 是 time.Since 的整数纳秒包装，统一计时入口。
func SinceNS(start time.Time) int64 { return time.Since(start).Nanoseconds() }
