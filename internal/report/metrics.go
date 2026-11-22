// Package report 汇总各阶段量化指标并输出机器可读 JSON 行。
package report

import (
	"encoding/json"
	"fmt"
	"math"
)

// Metrics 一次运行的量化结果。
type Metrics struct {
	Scenario         string  `json:"scenario,omitempty"`
	TargetKbps       int     `json:"target_kbps"`
	ActualKbps       float64 `json:"actual_kbps"`
	BitrateErrorPct  float64 `json:"bitrate_error_pct"`
	InputLUFS        float64 `json:"input_lufs"`
	OutputLUFS       float64 `json:"output_lufs"`
	PeakDBFS         float64 `json:"peak_dbfs"`
	SilenceRatio     float64 `json:"silence_ratio"`
	GapPackets       int     `json:"gap_packets"`
	DuplicatePackets int     `json:"duplicate_packets"`
	ReorderedPackets int     `json:"reordered_packets"`
	ManifestErrorMs  float64 `json:"manifest_error_ms"`
	IndexDigestMatch bool    `json:"index_digest_match"`
	RecoveryAction   string  `json:"recovery_action"`
	ResumedFromSeg   int     `json:"resumed_from_segment"`
	Error            string  `json:"error,omitempty"`
}

// Round 保留 3 位小数，保证重复运行输出一致。
func (m *Metrics) Round() {
	r := func(f float64) float64 {
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return f
		}
		return math.Round(f*1000) / 1000
	}
	m.ActualKbps = r(m.ActualKbps)
	m.BitrateErrorPct = r(m.BitrateErrorPct)
	m.InputLUFS = r(m.InputLUFS)
	m.OutputLUFS = r(m.OutputLUFS)
	m.PeakDBFS = r(m.PeakDBFS)
	m.SilenceRatio = r(m.SilenceRatio)
	m.ManifestErrorMs = r(m.ManifestErrorMs)
}

// JSON 输出单行 JSON。
func (m Metrics) JSON() string {
	m.Round()
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return string(b)
}
