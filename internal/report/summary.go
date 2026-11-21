// Package report 输出机器可读的交付摘要。
package report

import (
	"encoding/json"
	"io"
	"sort"
	"time"

	"mediadeliver/internal/fault"
)

// Summary 是终端输出的 JSON 摘要。
type Summary struct {
	Path                string     `json:"path"` // normal | corrupt | recover
	SyncErrorMs         float64    `json:"sync_error_ms"`
	PacketRecoveryRate  float64    `json:"packet_recovery_rate"`
	P95LatencyMs        float64    `json:"p95_latency_ms"`
	ParsedSegments      int        `json:"parsed_segments"`
	RecoveredSegments   int        `json:"recovered_segments"`
	FirstErrorOffset    int64      `json:"first_error_offset"`
	OutputSHA256        string     `json:"output_sha256"`
	Faults              fault.List `json:"faults,omitempty"`
}

// P95Ms 计算延迟样本的 P95（毫秒）。
func P95Ms(ds []time.Duration) float64 {
	if len(ds) == 0 {
		return 0
	}
	ms := make([]float64, len(ds))
	for i, d := range ds {
		ms[i] = float64(d) / float64(time.Millisecond)
	}
	sort.Float64s(ms)
	idx := int(0.95*float64(len(ms)-1) + 0.5)
	if idx >= len(ms) {
		idx = len(ms) - 1
	}
	return ms[idx]
}

// WriteJSON 以紧凑 JSON 输出摘要。
func WriteJSON(w io.Writer, s Summary) error {
	enc := json.NewEncoder(w)
	return enc.Encode(s)
}
