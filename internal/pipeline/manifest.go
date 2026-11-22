package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"fieldcut/internal/parse"
)

// Summary 是验收与演示使用的量化摘要。
type Summary struct {
	RoundtripRate   float64            `json:"roundtrip_rate"`
	KeyframeHitRate float64            `json:"keyframe_hit_rate"`
	Segments        int                `json:"segments"`
	SegmentSHA256   map[string]string  `json:"segment_sha256"`
	RecoveredBytes  int64              `json:"recovered_bytes"`
	SkippedBoxes    int                `json:"skipped_boxes"`
	SilenceMs       int64              `json:"silence_ms"`
	TimelineErrors  int                `json:"timeline_errors"`
	DegradedSegments int               `json:"degraded_segments"`
	DuplicateSegments int              `json:"duplicate_segments"`
	TotalEvents     int                `json:"total_events"`
	ExitCode        int                `json:"exit_code"`
}

// Manifest 是单次运行的完整交付清单。
type Manifest struct {
	Seed       int64             `json:"seed"`
	Source     string            `json:"source"`
	Summary    Summary           `json:"summary"`
	Segments   []*Segment        `json:"segments"`
	Events     []AssignedEvent   `json:"events"`
	Silence    []SilenceInterval `json:"silence"`
	Recoveries []parse.Boundary  `json:"recoveries"`
}

// Thresholds 是验收指标下限。
type Thresholds struct {
	RoundtripRate   float64
	KeyframeHitRate float64
	MinSegments     int
}

// DefaultThresholds 与任务验收要求一致。
func DefaultThresholds() Thresholds {
	return Thresholds{RoundtripRate: 0.98, KeyframeHitRate: 0.95, MinSegments: 4}
}

// BuildSummary 汇总切片、恢复报告与静音结果。
// roundtripRate = 时间戳成功映射的样本占比；keyframeHitRate = 落在确认
// 随机访问点上的安全切点占比。
func BuildSummary(segs []*Segment, rep *parse.Report,
	silenceMs int64, mappedSamples, totalSamples int,
	recovered int64) Summary {
	sum := Summary{
		Segments:       len(segs),
		SegmentSHA256:  make(map[string]string),
		RecoveredBytes: recovered,
		SkippedBoxes:   rep.SkippedBoxes,
		SilenceMs:      silenceMs,
		TimelineErrors: rep.TimelineErrors(),
	}
	if totalSamples > 0 {
		sum.RoundtripRate = float64(mappedSamples) / float64(totalSamples)
	}
	safeCuts, cuts := 0, 0
	for _, s := range segs {
		sum.SegmentSHA256[s.ID] = s.SHA256
		if s.Degraded {
			sum.DegradedSegments++
		}
		if s.DuplicateOf != "" {
			sum.DuplicateSegments++
		}
		cuts++
		if s.SafeCut {
			safeCuts++
		}
	}
	if cuts > 0 {
		sum.KeyframeHitRate = float64(safeCuts) / float64(cuts)
	}
	return sum
}

// Evaluate 对照阈值给出退出码：0 通过，4 指标未达标。
func (s *Summary) Evaluate(th Thresholds) int {
	if s.RoundtripRate+1e-9 < th.RoundtripRate ||
		s.KeyframeHitRate+1e-9 < th.KeyframeHitRate ||
		s.Segments < th.MinSegments {
		s.ExitCode = 4
		return 4
	}
	s.ExitCode = 0
	return 0
}

// Write 把清单以确定性 JSON 写入 outDir/manifest.json。
func (m *Manifest) Write(outDir string) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	// 固定片段与事件顺序，保证重跑字节一致。
	sort.Slice(m.Segments, func(i, j int) bool {
		return m.Segments[i].ID < m.Segments[j].ID
	})
	path := filepath.Join(outDir, "manifest.json")
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// ReadManifest 从目录读回清单，供 verify 重跑比对。
func ReadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
