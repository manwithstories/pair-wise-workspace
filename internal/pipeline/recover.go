// Package pipeline 以完整片段为单位保存检查点并支持中断恢复：
// 只有完整且哈希核验通过的片段才会提交，截断重跑时只重做未提交片段，
// 重复运行产出字节一致的清单与哈希。
package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"tsproof/internal/segment"
)

// CheckpointFileName 是检查点在输出目录中的固定文件名。
const CheckpointFileName = "checkpoint.json"

// ErrCheckpoint 表示检查点读写或状态不一致。
var ErrCheckpoint = errors.New("pipeline: checkpoint error")

// Checkpoint 记录已提交片段的可复算状态。
type Checkpoint struct {
	Version       int             `json:"version"`
	Seed          uint64          `json:"seed"`
	PerSegment    int             `json:"per_segment"`
	Committed     []segment.Hash  `json:"committed"`
	Manifest      []ManifestLine  `json:"manifest"`
	TotalPackets  int             `json:"total_packets"`
}

// ManifestLine 是已导出帧的清单行（与 render.ManifestEntry 结构对齐，
// 这里独立定义避免 pipeline 反向依赖渲染层）。
type ManifestLine struct {
	Seq       int    `json:"seq"`
	File      string `json:"file"`
	PTS27     int64  `json:"pts27"`
	PTSSecs   string `json:"pts_secs"`
	Key       bool   `json:"key"`
	Untrusted bool   `json:"untrusted"`
	AudioN    int    `json:"audio_blocks"`
	SHA256    string `json:"sha256"`
}

// Store 管理检查点的落盘、提交与恢复。
type Store struct {
	dir string
	cp  Checkpoint
}

// NewStore 创建或加载输出目录中的检查点。
func NewStore(dir string, seed uint64, perSegment int) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("%w: mkdir: %v", ErrCheckpoint, err)
	}
	s := &Store{dir: dir, cp: Checkpoint{
		Version:    1,
		Seed:       seed,
		PerSegment: perSegment,
	}}
	path := filepath.Join(dir, CheckpointFileName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: read: %v", ErrCheckpoint, err)
	}
	var loaded Checkpoint
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrCheckpoint, err)
	}
	// 种子或分片参数变化属于不同作业，拒绝混用旧检查点。
	if loaded.Seed != seed || loaded.PerSegment != perSegment || loaded.Version != 1 {
		return nil, fmt.Errorf("%w: seed/segment/version mismatch", ErrCheckpoint)
	}
	s.cp = loaded
	return s, nil
}

// CommittedCount 返回已提交片段数（恢复时从这里继续）。
func (s *Store) CommittedCount() int { return len(s.cp.Committed) }

// CommittedHashes 返回已提交片段摘要的副本。
func (s *Store) CommittedHashes() []segment.Hash {
	out := make([]segment.Hash, len(s.cp.Committed))
	copy(out, s.cp.Committed)
	return out
}

// CommitSegment 仅在片段完整且通过期望哈希核验后提交，并原子落盘检查点。
//
// 不变量：任何未通过核验的片段都不会进入 Committed，中断不会留下
// 半提交状态（先更新内存再原子重命名临时文件）。
func (s *Store) CommitSegment(got, want segment.Hash) error {
	if got.PacketTo-got.PacketFrom != want.PacketTo-want.PacketFrom ||
		got.SHA256 != want.SHA256 {
		return fmt.Errorf("%w: segment %d hash mismatch", ErrCheckpoint, want.Index)
	}
	if len(s.cp.Committed) != want.Index {
		return fmt.Errorf("%w: out-of-order commit got=%d have=%d",
			ErrCheckpoint, want.Index, len(s.cp.Committed))
	}
	s.cp.Committed = append(s.cp.Committed, want)
	return s.persist()
}

// AppendManifest 追加一条已导出帧清单并持久化。
func (s *Store) AppendManifest(line ManifestLine) error {
	s.cp.Manifest = append(s.cp.Manifest, line)
	return s.persist()
}

// SetTotalPackets 记录本次作业观测到的总包数并持久化。
func (s *Store) SetTotalPackets(n int) error {
	s.cp.TotalPackets = n
	return s.persist()
}

// Manifest 返回已提交清单副本。
func (s *Store) Manifest() []ManifestLine {
	out := make([]ManifestLine, len(s.cp.Manifest))
	copy(out, s.cp.Manifest)
	return out
}

// Snapshot 返回检查点快照副本。
func (s *Store) Snapshot() Checkpoint {
	cp := s.cp
	cp.Committed = s.CommittedHashes()
	cp.Manifest = s.Manifest()
	return cp
}

// persist 原子写入检查点（临时文件 + rename）。
func (s *Store) persist() error {
	raw, err := json.MarshalIndent(&s.cp, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encode: %v", ErrCheckpoint, err)
	}
	tmp := filepath.Join(s.dir, CheckpointFileName+".tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("%w: write tmp: %v", ErrCheckpoint, err)
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, CheckpointFileName)); err != nil {
		return fmt.Errorf("%w: rename: %v", ErrCheckpoint, err)
	}
	return nil
}
