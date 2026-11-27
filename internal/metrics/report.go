// Package metrics 统计验收指标并输出终端报告。
package metrics

import (
	"fmt"
	"io"
	"sort"
)

// 验收阈值。
const (
	SyncMaxMsLimit     = 20.0
	LossRecoveryMinPct = 90.0
	P95SegmentMsLimit  = 250.0
	WantFrames         = 120
)

// Report 验收报告。
type Report struct {
	SyncMaxMs       float64
	LossRecoveryPct float64
	P95SegmentMs    float64
	RollbackOK      bool
	RepeatHashEqual bool
	Frames          int
	SHA256          string
	StageCommits    map[string]int
	FirstErrSource  string
	FirstErrOffset  int64
	HasFirstErr     bool
}

// Percentile 计算百分位（最近秩法）。
func Percentile(ds []float64, p float64) float64 {
	if len(ds) == 0 {
		return 0
	}
	sorted := make([]float64, len(ds))
	copy(sorted, ds)
	sort.Float64s(sorted)
	rank := int(p / 100 * float64(len(sorted)))
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	if rank < 0 {
		rank = 0
	}
	return sorted[rank]
}

// Pass 判断全部验收指标是否通过。
func (r *Report) Pass() bool {
	return r.SyncMaxMs <= SyncMaxMsLimit &&
		r.LossRecoveryPct >= LossRecoveryMinPct &&
		r.P95SegmentMs <= P95SegmentMsLimit &&
		r.RollbackOK &&
		r.RepeatHashEqual &&
		r.Frames == WantFrames
}

// Print 输出终端报告。
func (r *Report) Print(w io.Writer) {
	fmt.Fprintf(w, "sync_max_ms=%.2f\n", r.SyncMaxMs)
	fmt.Fprintf(w, "loss_recovery_pct=%.1f\n", r.LossRecoveryPct)
	fmt.Fprintf(w, "p95_segment_ms=%.2f\n", r.P95SegmentMs)
	fmt.Fprintf(w, "rollback_ok=%t\n", r.RollbackOK)
	fmt.Fprintf(w, "repeat_hash_equal=%t\n", r.RepeatHashEqual)
	fmt.Fprintf(w, "frames=%d\n", r.Frames)
	fmt.Fprintf(w, "sha256=%s\n", r.SHA256)
	stages := []string{"parse", "recover", "mix", "render"}
	io.WriteString(w, "stage_commits")
	for _, s := range stages {
		fmt.Fprintf(w, " %s=%d", s, r.StageCommits[s])
	}
	io.WriteString(w, "\n")
	if r.HasFirstErr {
		fmt.Fprintf(w, "first_error source=%s offset=%d\n", r.FirstErrSource, r.FirstErrOffset)
	} else {
		io.WriteString(w, "first_error none\n")
	}
}
