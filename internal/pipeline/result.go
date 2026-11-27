// Package pipeline 编排有界并发的解读-传输-恢复-同步-再封装管线，
// 单文件失败不阻断批次，结构损坏或取消时绝不发布半成品文件。
package pipeline

import (
	"bcastqc/internal/media"
)

// Metrics 是单样例对外暴露的验收统计口径。
type Metrics struct {
	Case             string `json:"case"`
	SyncMaxMS        int64  `json:"sync_max_ms"`
	Lost             int    `json:"lost"`
	Recovered        int    `json:"recovered"`
	RecoveryPct      float64 `json:"recovery_pct"`
	LatencyP95MS     int64  `json:"latency_p95_ms"`
	PeakBufferBytes  int64  `json:"peak_buffer_bytes"`
	OutputSHA256     string `json:"output_sha256"`
	FirstMissingOff  int64  `json:"first_missing_offset"`
	Rejected         bool   `json:"rejected"`
	Recoverable      bool   `json:"recoverable"`
	ErrKind          string `json:"err_kind,omitempty"`
	ErrDetail        string `json:"err_detail,omitempty"`
}

// Result 是单个样例的处理结果。
type Result struct {
	SpecName string
	Metrics  Metrics
	// Output 为再封装字节；被拒绝时为 nil。
	Output []byte
	Err    error
}

// Failed 区分可继续批次的单文件失败。
func (r Result) Failed() bool { return r.Err != nil }

// firstMissingOffset 从错误链提取首个缺失偏移，未知为 -1。
func firstMissingOffset(err error) int64 {
	if err == nil {
		return -1
	}
	return media.AsError(err, media.StageParse, media.KindTruncated).Offset
}
