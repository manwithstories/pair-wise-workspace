// Package store 记录片段哈希清单与可续跑状态。
package store

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediatool/internal/clock"
	"mediatool/internal/video"
)

// SegmentRecord 是清单中一个片段的记录。
type SegmentRecord struct {
	RepID    int    `json:"rep_id"`
	SegID    int    `json:"seg_id"`
	File     string `json:"file"`
	StartMs  int64  `json:"start_ms"`
	EndMs    int64  `json:"end_ms"`
	SHA256   string `json:"sha256"`
	Complete bool   `json:"complete"`
}

// Manifest 是一次运行的完整清单。
type Manifest struct {
	Version      int             `json:"version"`
	InputSHA256  string          `json:"input_sha256"`
	ConfigSHA256 string          `json:"config_sha256"`
	Segments     []SegmentRecord `json:"segments"`
	Faults       clock.Faults    `json:"faults"`
	Color        []video.Record  `json:"color"`
}

// HashBytes 计算字节串的 SHA-256 十六进制。
func HashBytes(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

// HashFile 计算文件的 SHA-256 十六进制。
func HashFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return HashBytes(b), nil
}

// AddSegment 追加一条片段记录。
func (m *Manifest) AddSegment(r SegmentRecord) { m.Segments = append(m.Segments, r) }

// Save 将清单写为 JSON。
func (m *Manifest) Save(path string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Load 读取清单。
func Load(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Resume 校验输入哈希与配置哈希，返回已完成（完整且文件哈希匹配）的片段集合。
// 哈希不匹配的记录不被当作已完成；输入或配置变化会导致无法续跑。
func (m *Manifest) Resume(inputHash, cfgHash string) (map[[2]int]bool, error) {
	done := map[[2]int]bool{}
	if m.InputSHA256 != inputHash {
		return done, fmt.Errorf("input hash changed: cannot resume")
	}
	if m.ConfigSHA256 != cfgHash {
		return done, fmt.Errorf("config hash changed: cannot resume")
	}
	for _, r := range m.Segments {
		if !r.Complete {
			continue
		}
		h, err := HashFile(r.File)
		if err != nil || h != r.SHA256 {
			continue // 不完整或损坏的输出不算完成
		}
		done[[2]int{r.RepID, r.SegID}] = true
	}
	return done, nil
}

// Verify 独立复算所有已完成片段的文件哈希，返回不一致的记录。
func (m *Manifest) Verify() ([]SegmentRecord, error) {
	var bad []SegmentRecord
	for _, r := range m.Segments {
		if !r.Complete {
			continue
		}
		h, err := HashFile(r.File)
		if err != nil {
			return nil, err
		}
		if h != r.SHA256 {
			bad = append(bad, r)
		}
	}
	return bad, nil
}

// IncompleteCount 返回未完成片段数。
func (m *Manifest) IncompleteCount() int {
	n := 0
	for _, r := range m.Segments {
		if !r.Complete {
			n++
		}
	}
	return n
}

// ManifestPath 返回目录下清单文件路径。
func ManifestPath(dir string) string { return filepath.Join(dir, "manifest.json") }
