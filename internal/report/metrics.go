// Package report 汇总处理后的码率与质量指标，产出确定性、可复核的终端摘要。
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"

	"mediacheck/internal/core"
	"mediacheck/internal/model"
)

// Metrics 是一次处理的完整指标集合，字段名与验收 token 对齐。
type Metrics struct {
	Status           string  `json:"status"`
	Seed             int64   `json:"seed"`
	TargetKBPS       int     `json:"target_kbps"`
	ActualKBPS       int     `json:"actual_kbps"`
	BitrateDeltaPct  float64 `json:"bitrate_delta_pct"`
	SamplesIn        int64   `json:"samples_in"`
	SamplesOut       int64   `json:"samples_out"`
	PeakDBFS         float64 `json:"peak_dbfs"`
	LUFS             float64 `json:"lufs"`
	ColorConflicts   int     `json:"color_conflicts"`
	ManifestAlignMS  float64 `json:"manifest_alignment_ms"`
	FragmentGaps     int     `json:"fragment_gaps"`
	SilentFrames     int     `json:"silent_frames"`
	ClippedSamples   int     `json:"clipped_samples"`
	ShortFrames      int     `json:"short_frames"`
	UnknownBoxes     int     `json:"unknown_boxes"`
	SummarySHA       string  `json:"summary_sha256"`
	RejectReason     string  `json:"reject_reason,omitempty"`
}

// Inputs 聚合各子系统结果，避免报告模块直接依赖具体编排细节。
type Inputs struct {
	Seed         int64
	TargetKBPS   int
	Normalize    *core.NormalizeResult
	Pack         *core.PackResult
	Color        core.ColorDecision
	AlignMS      float64
	Gaps         int
	UnknownBoxes int
}

// Build 计算码率偏差并保证所有浮点字段有限（杜绝 NaN 进入摘要）。
func Build(in Inputs) Metrics {
	m := Metrics{
		Status: "ok", Seed: in.Seed, TargetKBPS: in.TargetKBPS,
		ColorConflicts: in.Color.Conflicts,
		ManifestAlignMS: sanitize(in.AlignMS),
		FragmentGaps:    in.Gaps,
		UnknownBoxes:    in.UnknownBoxes,
	}
	if in.Normalize != nil {
		m.SamplesIn = in.Normalize.SamplesIn
		m.SamplesOut = in.Normalize.SamplesOut
		m.PeakDBFS = sanitize(in.Normalize.PeakDBFS)
		m.LUFS = sanitize(in.Normalize.Stats.IntegratedLUFS)
		m.SilentFrames = in.Normalize.Stats.SilentFrames
		m.ClippedSamples = in.Normalize.Stats.ClippedSamples
		m.ShortFrames = in.Normalize.Stats.ShortFrames
	}
	if in.Pack != nil {
		m.ActualKBPS = in.Pack.ActualKBPS
	}
	if in.TargetKBPS > 0 {
		m.BitrateDeltaPct = sanitize(math.Abs(float64(m.ActualKBPS-in.TargetKBPS)) * 100 / float64(in.TargetKBPS))
	}
	m.SummarySHA = m.computeSHA()
	return m
}

// Reject 构造损坏输入的拒绝指标（不产出成功清单场景）。
func Reject(seed int64, reason model.ErrorCategory) Metrics {
	m := Metrics{
		Status: "rejected", Seed: seed, RejectReason: string(reason),
		PeakDBFS: -96, LUFS: -70,
	}
	m.SummarySHA = m.computeSHA()
	return m
}

// computeSHA 对规范化 JSON（剔除 summary 自身）求 sha256，保证重复运行稳定。
func (m Metrics) computeSHA() string {
	cp := m
	cp.SummarySHA = ""
	data, _ := json.Marshal(cp)
	sum := sha256.Sum256(canonicalJSON(data))
	return hex.EncodeToString(sum[:])
}

// canonicalJSON 借助 decode/encode 做键排序，消除 map 顺序不确定性。
func canonicalJSON(data []byte) []byte {
	var any interface{}
	if err := json.Unmarshal(data, &any); err != nil {
		return data
	}
	sorted, err := json.Marshal(any)
	if err != nil {
		return data
	}
	return sorted
}

func sanitize(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// SortedKeys 返回指标 JSON 键的稳定顺序，供终端表格输出。
func SortedKeys() []string {
	return []string{
		"status", "seed", "target_kbps", "actual_kbps", "bitrate_delta_pct",
		"samples_in", "samples_out", "peak_dbfs", "lufs", "color_conflicts",
		"manifest_alignment_ms", "fragment_gaps", "silent_frames", "clipped_samples",
		"short_frames", "unknown_boxes", "summary_sha256",
	}
}
