// Package audit 只读汇总其它模块的输出：轮换次数、逐段 key_id 与失败原因，
// 并按 key_id 分组统计其覆盖的段数。本包不参与加解密与授权判定。
package audit

import (
	"encoding/json"
	"os"
	"sort"
)

// SegmentStatus 是单段的最终处理结果。
type SegmentStatus struct {
	Index  int    `json:"index"`
	KeyID  string `json:"key_id"`
	Epoch  int64  `json:"epoch"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// Failure 记录一次失败的段号与原因码。
type Failure struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// KeyCoverage 是单个 key_id 覆盖的段统计。
type KeyCoverage struct {
	KeyID        string `json:"key_id"`
	Epoch        int64  `json:"epoch"`
	SegmentCount int    `json:"segment_count"`
}

// Report 是审计报告的完整结构，也是 verify 的 stdout/落盘格式。
type Report struct {
	RotationCount int             `json:"rotation_count"`
	SegmentCount  int             `json:"segment_count"`
	Segments      []SegmentStatus `json:"segments"`
	Failures      []Failure       `json:"failures"`
	KeyCoverage   []KeyCoverage   `json:"key_coverage"`
}

// KeyCoverageView 从清单视角按 key_id 分组统计覆盖段数（不依赖解密结果）。
func KeyCoverageView(segments []SegmentStatus) []KeyCoverage {
	order := []string{}
	index := map[string]int{}
	epochs := map[string]int64{}
	for _, seg := range segments {
		pos, ok := index[seg.KeyID]
		if !ok {
			pos = len(order)
			index[seg.KeyID] = pos
			order = append(order, seg.KeyID)
			epochs[seg.KeyID] = seg.Epoch
		}
		_ = pos
	}
	counts := make([]int, len(order))
	for _, seg := range segments {
		counts[index[seg.KeyID]]++
	}
	view := make([]KeyCoverage, 0, len(order))
	for i, keyID := range order {
		view = append(view, KeyCoverage{
			KeyID:        keyID,
			Epoch:        epochs[keyID],
			SegmentCount: counts[i],
		})
	}
	sort.Slice(view, func(i, j int) bool { return view[i].Epoch < view[j].Epoch })
	return view
}

// Build 根据逐段处理结果汇总报告，并自动计算 key 覆盖分组。
func Build(rotationCount int, segments []SegmentStatus, failures []Failure) *Report {
	if segments == nil {
		segments = []SegmentStatus{}
	}
	if failures == nil {
		failures = []Failure{}
	}
	return &Report{
		RotationCount: rotationCount,
		SegmentCount:  len(segments),
		Segments:      segments,
		Failures:      failures,
		KeyCoverage:   KeyCoverageView(segments),
	}
}

// Save 以缩进 JSON 落盘报告。
func (r *Report) Save(path string) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// JSON 返回单行紧凑 JSON，用于 stdout。
func (r *Report) JSON() string {
	raw, _ := json.Marshal(r)
	return string(raw)
}

// Load 读取已落盘的报告，供独立的 audit 子命令使用。
func Load(path string) (*Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
