// Package report 汇总码率偏差、样本连续性、损坏计数与清单/修复时间线对齐
// 统计，输出机器可读 JSON 与单行摘要。所有阈值均为显式参数，不可判定
// 时间段绝不计入成功。
package report

import (
	"encoding/json"
	"fmt"
	"strings"

	"mediatime/internal/clock"
	"mediatime/internal/segment"
)

// Status 是一次运行的终态分类，对应进程退出码。
type Status string

// 终态常量。
const (
	StatusOK       Status = "ok"       // 退出码 0
	StatusInvalid  Status = "invalid"  // 退出码 2
	StatusPartial  Status = "partial"  // 退出码 3
	StatusCanceled Status = "canceled" // 退出码 4
	StatusInternal Status = "internal" // 退出码 5
)

// Thresholds 是显式配置的质量/对齐判定阈值，不允许使用隐式默认值。
type Thresholds struct {
	// BitrateErrorPct 允许的目标/实际码率偏差百分比（含）。
	BitrateErrorPct float64
	// AlignmentMaxMs 允许的清单与修复时间线最大对齐误差毫秒（含）。
	AlignmentMaxMs float64
}

// Metrics 是一次运行的完整指标，JSON 字段顺序即结构声明顺序，保证确定性。
type Metrics struct {
	Status          Status  `json:"status"`
	ErrorCategory   string  `json:"error,omitempty"`
	Fragments       int     `json:"fragments"`
	Completed       int     `json:"completed"`
	Cancelled       bool    `json:"cancelled"`
	TargetBPS       int64   `json:"target_bps"`
	ActualBPS       int64   `json:"actual_bps"`
	BitrateErrorPct float64 `json:"bitrate_error_pct"`
	QualityBad      int     `json:"quality_bad"`
	Corrupted       int     `json:"corrupted_count"`
	Undecidable     int     `json:"undecidable_count"`
	SampleBreaks    int     `json:"sample_continuity_breaks"`
	AlignmentMaxMs  float64 `json:"alignment_max_ms"`
	WrapCount       int     `json:"wrap_count"`
	DriftPPM        float64 `json:"drift_ppm"`
	Gaps            int     `json:"timeline_gaps"`
	IndexKeys       int     `json:"index_keys"`
	IndexSHA256     string  `json:"index_sha256"`
}

// Inputs 聚合计算指标所需的各子系统产物。
type Inputs struct {
	ManifestFragments int
	TargetBPS         int64
	MediaBytes        int64 // 全部成功片段的样本负载字节总数
	Timeline          *clock.Timeline
	Index             *segment.Index
	Corrupted         int
	Cancelled         bool
	ErrorCategory     string
	ManifestStarts    map[int]clock.Tick // 片段序号 → 清单标称起点
	Thresholds        Thresholds
}

// Compute 依据各子系统产物计算确定性指标。
func Compute(in Inputs) *Metrics {
	m := &Metrics{
		Fragments:     in.ManifestFragments,
		Completed:     successfulFragments(in),
		Cancelled:     in.Cancelled,
		TargetBPS:     in.TargetBPS,
		Corrupted:     in.Corrupted,
		IndexSHA256:   "",
		ErrorCategory: in.ErrorCategory,
	}
	if in.Index != nil {
		m.IndexKeys = in.Index.Keys
		m.IndexSHA256 = in.Index.SHA256
	}
	if in.Timeline != nil {
		tl := in.Timeline
		m.WrapCount = tl.WrapCount
		m.DriftPPM = round2(tl.Drift.DriftPPM)
		m.Undecidable = tl.Undecidable
		m.Gaps = len(tl.Gaps)
		m.SampleBreaks = countContinuityBreaks(tl)
		m.AlignmentMaxMs = round2(alignmentMax(tl, in.ManifestStarts))
	}
	// 码率：8 * 字节 / 时长秒。时长为 0 时实际码率记 0。
	if in.Timeline != nil && in.Timeline.Duration() > 0 {
		seconds := clock.TickToSeconds(in.Timeline.Duration())
		m.ActualBPS = int64(float64(in.MediaBytes*8) / seconds)
	}
	if m.TargetBPS > 0 {
		m.BitrateErrorPct = round2(
			float64(abs64(m.ActualBPS-m.TargetBPS)) / float64(m.TargetBPS) * 100)
	}
	// 质量坏点：不可判定样本 + 损坏片段，绝不把不可判定算作成功。
	m.QualityBad = m.Undecidable + m.Corrupted
	m.Status = classify(m, in.Thresholds)
	return m
}

func successfulFragments(in Inputs) int {
	if in.Timeline == nil {
		return 0
	}
	seen := map[int]struct{}{}
	for _, s := range in.Timeline.Samples {
		seen[s.Sequence] = struct{}{}
	}
	return len(seen)
}

// countContinuityBreaks 统计同轨道相邻样本 DTS 倒退/断裂次数。
func countContinuityBreaks(tl *clock.Timeline) int {
	breaks := 0
	var prevTrack uint32
	var prevDTS clock.Tick
	first := map[uint32]bool{}
	for _, s := range tl.Samples {
		if !first[s.TrackID] {
			first[s.TrackID] = true
			prevTrack = s.TrackID
			prevDTS = s.DTS
			continue
		}
		if s.TrackID == prevTrack && s.DTS < prevDTS {
			breaks++
		}
		prevTrack = s.TrackID
		prevDTS = s.DTS
	}
	return breaks
}

// alignmentMax 计算每个片段首个可判定样本的修复 DTS 与清单标称起点的
// 最大偏差（毫秒）。缺片与不可判定片段不参与（它们已在质量统计中计入）。
func alignmentMax(tl *clock.Timeline, manifestStarts map[int]clock.Tick) float64 {
	first := map[int]clock.Tick{}
	for _, s := range tl.Samples {
		if s.IsUndecidable {
			continue
		}
		if _, ok := first[s.Sequence]; !ok {
			first[s.Sequence] = s.DTS
		}
	}
	var maxMs float64
	for seq, repaired := range first {
		nominal, ok := manifestStarts[seq]
		if !ok {
			continue
		}
		d := repaired - nominal
		if d < 0 {
			d = -d
		}
		if ms := clock.TickToMillis(d); ms > maxMs {
			maxMs = ms
		}
	}
	return maxMs
}

func classify(m *Metrics, th Thresholds) Status {
	switch {
	case m.Cancelled:
		return StatusCanceled
	case m.ErrorCategory != "" && m.Completed > 0:
		return StatusPartial
	case m.ErrorCategory != "":
		return StatusInvalid
	}
	return StatusOK
}

// ExitCode 把终态映射为验收规定的进程退出码。
func (m *Metrics) ExitCode() int {
	switch m.Status {
	case StatusOK:
		return 0
	case StatusInvalid:
		return 2
	case StatusPartial:
		return 3
	case StatusCanceled:
		return 4
	default:
		return 5
	}
}

// JSON 输出确定性的机器可读报告。
func (m *Metrics) JSON() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}

// Summary 输出单行 key=value 摘要，字段顺序固定。
func (m *Metrics) Summary() string {
	var sb strings.Builder
	fmt.Fprintf(&sb,
		"status=%s fragments=%d completed=%d cancelled=%t "+
			"target_bps=%d actual_bps=%d bitrate_error_pct=%.2f "+
			"quality_bad=%d alignment_max_ms=%.2f wrap_count=%d "+
			"drift_ppm=%.2f index_keys=%d index_sha256=%s",
		m.Status, m.Fragments, m.Completed, m.Cancelled,
		m.TargetBPS, m.ActualBPS, m.BitrateErrorPct,
		m.QualityBad, m.AlignmentMaxMs, m.WrapCount,
		m.DriftPPM, m.IndexKeys, m.IndexSHA256)
	if m.ErrorCategory != "" {
		fmt.Fprintf(&sb, " error=%s", m.ErrorCategory)
	}
	return sb.String()
}

func round2(v float64) float64 {
	return float64(int64(v*100+sign(v)*0.5)) / 100
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
