package scan

import (
	"encoding/json"
	"os"
	"path/filepath"

	"mediaverify/internal/clock"
	"mediaverify/internal/fault"
	"mediaverify/internal/manifest"
)

// IndexedSegment 索引中的一个分段。
type IndexedSegment struct {
	RepID    string      `json:"rep_id"`
	Index    int         `json:"index"`
	URI      string      `json:"uri"`
	Start    clock.Ticks `json:"start_ns"`
	Duration clock.Ticks `json:"duration_ns"`
	Digest   string      `json:"digest"`
	Size     int64       `json:"size"`
}

// Index 绑定全部输入摘要的分段索引。
type Index struct {
	Version      int              `json:"version"`
	ManifestPath string           `json:"manifest_path"`
	InputDigest  string           `json:"input_digest"` // 全部输入（清单+分段）的组合摘要
	Segments     []IndexedSegment `json:"segments"`
	Checksum     string           `json:"checksum"` // 内容校验，检测索引截断
}

// Build 扫描清单引用的全部分段并生成索引。分段缺失/不可读即报错。
func Build(prog *manifest.Program, dir, manifestPath string) (*Index, error) {
	idx := &Index{Version: 1, ManifestPath: manifestPath}
	digests := map[string]string{}
	md, _, err := DigestFile(manifestPath)
	if err != nil {
		return nil, fault.New(fault.KindIndex, fault.Fail, manifestPath, 0, -1, "digest manifest: %v", err)
	}
	digests[filepath.Base(manifestPath)] = md
	for _, rep := range prog.Representations {
		for _, seg := range rep.Segments {
			p := filepath.Join(dir, seg.URI)
			d, size, err := DigestFile(p)
			if err != nil {
				return nil, fault.New(fault.KindIndex, fault.Fail, p, 0, seg.Index,
					"segment unreadable: %v", err)
			}
			digests[seg.URI] = d
			idx.Segments = append(idx.Segments, IndexedSegment{
				RepID: seg.RepID, Index: seg.Index, URI: seg.URI,
				Start: seg.Start, Duration: seg.Duration, Digest: d, Size: size,
			})
		}
	}
	idx.InputDigest = CombineDigests(digests)
	idx.Checksum = idx.contentChecksum()
	return idx, nil
}

func (idx *Index) contentChecksum() string {
	b, _ := json.Marshal(struct {
		V int              `json:"v"`
		M string           `json:"m"`
		D string           `json:"d"`
		S []IndexedSegment `json:"s"`
	}{idx.Version, idx.ManifestPath, idx.InputDigest, idx.Segments})
	return DigestBytes(b)
}

// Save 写索引文件（JSON）。
func (idx *Index) Save(path string) error {
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// Load 读索引并校验完整性；截断/篡改的索引返回 Rescan 策略错误。
func Load(path string) (*Index, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fault.New(fault.KindIndex, fault.Rescan, path, 0, -1, "read index: %v", err)
	}
	var idx Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, fault.New(fault.KindIndex, fault.Rescan, path, 0, -1,
			"index truncated or corrupt: %v", err)
	}
	if idx.Checksum == "" || idx.Checksum != idx.contentChecksum() {
		return nil, fault.New(fault.KindIndex, fault.Rescan, path, 0, -1,
			"index checksum mismatch (truncated or tampered)")
	}
	return &idx, nil
}

// Verify 重新计算全部输入摘要并与索引绑定值比对；
// 任一输入变化即拒绝旧索引，要求重扫。
func (idx *Index) Verify(dir string) error {
	digests := map[string]string{}
	md, _, err := DigestFile(idx.ManifestPath)
	if err != nil {
		return fault.New(fault.KindIndex, fault.Rescan, idx.ManifestPath, 0, -1, "manifest gone: %v", err)
	}
	digests[filepath.Base(idx.ManifestPath)] = md
	for _, seg := range idx.Segments {
		p := filepath.Join(dir, seg.URI)
		d, _, err := DigestFile(p)
		if err != nil {
			return fault.New(fault.KindIndex, fault.Rescan, p, 0, seg.Index, "segment gone: %v", err)
		}
		digests[seg.URI] = d
	}
	if got := CombineDigests(digests); got != idx.InputDigest {
		return fault.New(fault.KindIndex, fault.Rescan, idx.ManifestPath, 0, -1,
			"input digest mismatch: index is stale, rescan required")
	}
	return nil
}
