// Package manifest 写出归档结果清单。
//
// 不变量：
//   - 清单只读取 journal 中“已原子提交”的片段，pending/interrupted 不入单；
//   - 写出采用临时文件 + fsync + 原子 rename，崩溃后只会看到旧版或新版；
//   - 序列化字段顺序固定（排序键），保证重复运行的清单字节级一致；
//   - 清单整体带 sha256，供续跑前后比对。
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"

	"archivemedia/internal/journal"
	"archivemedia/internal/model"
)

// ManifestFileName 是清单在根目录下的固定文件名。
const ManifestFileName = "manifest.json"

// SegmentEntry 是清单中的一个片段条目（只含已提交数据）。
type SegmentEntry struct {
	SegmentID       string            `json:"segment_id"`
	SourceID        string            `json:"source_id"`
	IdempotencyKey  string            `json:"idempotency_key"`
	AdapterName     string            `json:"adapter_name"`
	AdapterVersion  string            `json:"adapter_version"`
	FilePath        string            `json:"file_path"`
	FileSha256      string            `json:"file_sha256"`
	StartUs         int64             `json:"start_us"`
	EndUs           int64             `json:"end_us"`
	ProvenanceSha256 string           `json:"provenance_sha256"`
	Params          map[string]string `json:"params"`
}

// Manifest 是完整清单。
type Manifest struct {
	SchemaVersion string         `json:"schema_version"`
	RunID         string         `json:"run_id"`
	GeneratedAt   string         `json:"generated_at"`
	Segments      []SegmentEntry `json:"segments"`
	// 派生统计，供核验与终端报告直接读取。
	SegmentCount int    `json:"segment_count"`
	ManifestSha  string `json:"manifest_sha256,omitempty"`
}

// Build 从已提交的 journal 记录构造清单（确定性排序）。
func Build(records []*journal.SegmentRecord, runID, generatedAt string) Manifest {
	entries := make([]SegmentEntry, 0, len(records))
	for _, r := range records {
		entries = append(entries, SegmentEntry{
			SegmentID: r.SegmentID, SourceID: r.SourceID,
			IdempotencyKey: r.IdempotencyKey,
			AdapterName: r.AdapterName, AdapterVersion: r.AdapterVersion,
			FilePath: r.FilePath, FileSha256: r.FileSha256,
			StartUs: r.StartUs, EndUs: r.EndUs,
			ProvenanceSha256: r.Provenance,
			Params: cloneParams(r.Params),
		})
	}
	// 按幂等键排序，保证与提交顺序无关的确定性输出。
	sort.Slice(entries, func(i, j int) bool { return entries[i].IdempotencyKey < entries[j].IdempotencyKey })
	return Manifest{
		SchemaVersion: "1.0", RunID: runID, GeneratedAt: generatedAt,
		Segments: entries, SegmentCount: len(entries),
	}
}

func cloneParams(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// marshal 用固定缩进与换行结尾序列化，保证字节级可重复。
func marshal(m Manifest) ([]byte, error) {
	// 先不含 ManifestSha 序列化，再把该哈希单独计算后由调用方写入报告。
	m.ManifestSha = ""
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, model.Wrap(model.KindCheckpoint, err, "manifest: marshal")
	}
	return append(data, '\n'), nil
}

// ContentSha256 计算清单内容哈希（不含自身 ManifestSha 字段）。
func ContentSha256(m Manifest) (string, error) {
	data, err := marshal(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// WriteAtomically 原子写出清单到 root/manifest.json。
//
// 流程：写临时文件 -> Sync -> 关闭 -> rename，保证读端永不看到半文件。
func WriteAtomically(root string, m Manifest) (string, error) {
	final, err := journal.SafePath(root, ManifestFileName)
	if err != nil {
		return "", err
	}
	data, err := marshal(m)
	if err != nil {
		return "", err
	}
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", model.Wrap(model.KindCheckpoint, err, "manifest: create tmp")
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", model.Wrap(model.KindCheckpoint, err, "manifest: write tmp")
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", model.Wrap(model.KindCheckpoint, err, "manifest: fsync")
	}
	if err := f.Close(); err != nil {
		return "", model.Wrap(model.KindCheckpoint, err, "manifest: close")
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", model.Wrap(model.KindCheckpoint, err, "manifest: rename")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Read 读回清单，供 verify/resume 比对。
func Read(root string) (*Manifest, error) {
	p, err := journal.SafePath(root, ManifestFileName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, model.Wrap(model.KindCheckpoint, err, "manifest: read")
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, model.Wrap(model.KindCheckpoint, err, "manifest: unmarshal")
	}
	return &m, nil
}

// EnsureDir 确认根目录存在（受安全路径约束）。
func EnsureDir(root, sub string) (string, error) {
	p, err := journal.SafePath(root, sub)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", model.Wrap(model.KindPathViolation, err, "manifest: mkdir %s", p)
	}
	return p, nil
}
