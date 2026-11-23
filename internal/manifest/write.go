// Package manifest 输出轨道、时长与码率清单。
package manifest

import (
	"encoding/json"
	"io"
	"sort"
)

// Track 是清单中的一条轨道。
type Track struct {
	Number      uint64  `json:"number"`
	Type        string  `json:"type"` // "video" / "audio"
	Codec       string  `json:"codec"`
	BaseNs      int64   `json:"base_ns"` // 轨道时间基准
	BitrateKbps float64 `json:"bitrate_kbps"`
}

// Manifest 是处理结果的清单。
type Manifest struct {
	StartNs      int64   `json:"start_ns"`
	EndNs        int64   `json:"end_ns"`
	DurationNs   int64   `json:"duration_ns"`
	SourceSHA256 string  `json:"source_sha256"`
	Tracks       []Track `json:"tracks"`
}

// Write 以确定性字段顺序输出 JSON 清单（轨道按编号排序）。
func Write(w io.Writer, m Manifest) error {
	tracks := make([]Track, len(m.Tracks))
	copy(tracks, m.Tracks)
	sort.Slice(tracks, func(i, j int) bool { return tracks[i].Number < tracks[j].Number })
	m.Tracks = tracks
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}
