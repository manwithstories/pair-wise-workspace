// Package manifest 生成与随机访问索引共用同一时间基准的 JSON 交付清单。
// 所有写操作先落临时文件再原子替换，避免半成品清单被验证阶段读取。
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"pairflow/internal/clock"
)

// Track 是清单中的单轨道条目。
type Track struct {
	TrackID          uint32                 `json:"track_id"`
	DeviceID         string                 `json:"device_id"`
	SampleRate       uint32                 `json:"sample_rate"`
	Channels         uint16                 `json:"channels"`
	BitsPerSample    uint16                 `json:"bits_per_sample"`
	StartSample      int64                  `json:"start_sample"`
	EndSample        int64                  `json:"end_sample"`
	TotalSamples     int64                  `json:"total_samples"`
	MissingSamples   int64                  `json:"missing_samples"`
	EntryCount       int                    `json:"entry_count"`
	TargetBitrateBps int64                  `json:"target_bitrate_bps"`
	ActualBitrateBps int64                  `json:"actual_bitrate_bps"`
	SourceOffsets    []int64                `json:"source_offsets"`
	UncertainRanges  []clock.UncertainRange `json:"uncertain_ranges,omitempty"`
}

// Manifest 是完整交付清单，时间范围与索引容器的采样时间线完全同源。
type Manifest struct {
	Version        int     `json:"version"`
	CreatedNano    int64   `json:"created_nano"`
	InputDigest    string  `json:"input_digest"`
	TimelineBaseMS float64 `json:"timeline_base_ms"` // 全清单统一时间基准（采样点 0）
	Tracks         []Track `json:"tracks"`
}

// WriteAtomic 在 outDir 内写临时文件后原子替换为 manifest.json。
func WriteAtomic(outDir, name string, m *Manifest) (string, error) {
	if err := ensureWithin(outDir, name); err != nil {
		return "", err
	}
	final := filepath.Join(outDir, name)
	tmp, err := os.CreateTemp(outDir, ".manifest-*.tmp")
	if err != nil {
		return "", clock.Wrap("manifest", clock.KindPathEscape, "", 0,
			clock.NoSeq, "create temp manifest failed", "检查输出目录权限", err)
	}
	tmpName := tmp.Name()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return "", clock.Wrap("manifest", clock.KindPathEscape, "", 0,
			clock.NoSeq, "atomic rename failed", "检查输出目录", err)
	}
	return final, nil
}

// Load 读取并解析清单。
func Load(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, clock.Wrap("manifest", clock.KindManifestAlignment, "", 0,
			clock.NoSeq, "manifest unreadable", "确认清单路径", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, clock.Wrap("manifest", clock.KindManifestAlignment, "", 0,
			clock.NoSeq, "manifest json invalid", "重新生成清单", err)
	}
	return &m, nil
}

// ensureWithin 防止路径操作越出用户指定的输出目录。
func ensureWithin(outDir, name string) error {
	absOut, err := filepath.Abs(outDir)
	if err != nil {
		return err
	}
	absName, err := filepath.Abs(filepath.Join(outDir, name))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absOut, absName)
	if err != nil || rel == ".." || filepath.IsAbs(rel) ||
		(len(rel) >= 3 && rel[:3] == ".."+string(os.PathSeparator)) {
		return clock.New("manifest", clock.KindPathEscape, "", 0, clock.NoSeq,
			fmt.Sprintf("path %q escapes output directory", name), "仅允许写入输出目录内")
	}
	return nil
}

// TrackByID 按轨道 ID 查找条目。
func (m *Manifest) TrackByID(id uint32) *Track {
	for i := range m.Tracks {
		if m.Tracks[i].TrackID == id {
			return &m.Tracks[i]
		}
	}
	return nil
}
