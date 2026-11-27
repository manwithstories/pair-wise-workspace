// Package engine 把读取、时钟恢复、索引扫描与音频转换串成流式流水线，
// 并通过原子检查点支持进程中断或写盘失败后的断点续跑。
package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"relay/internal/media"
)

// Checkpoint 是一次处理进度的可持久化状态。
type Checkpoint struct {
	InputDigest  string `json:"input_digest"`  // 输入文件 sha256，不符则作废
	ParamDigest  string `json:"param_digest"`  // 参数摘要，不符则作废
	OutOffset    int64  `json:"out_offset"`    // 已确认落盘的输出字节数
	EntriesDone  int    `json:"entries_done"`  // 已处理的索引条目数
	PPM          int    `json:"ppm"`           // 时钟漂移状态
	WrapCount    uint64 `json:"wrap_count"`    // 时间戳回绕计数
	GainDB       float64 `json:"gain_db"`      // 响度增益（两遍法第一遍求得）
	Phase        string `json:"phase"`         // measure / render / done
}

// CheckpointStore 原子读写检查点文件。
type CheckpointStore struct{ path string }

// NewCheckpointStore 指定检查点路径。
func NewCheckpointStore(path string) *CheckpointStore {
	return &CheckpointStore{path: path}
}

// Save 经临时文件 + rename 原子提交，保证检查点始终完整可读。
func (s *CheckpointStore) Save(cp *Checkpoint) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return &media.IOError{Op: "mkdir", Path: s.path, Reason: err.Error()}
	}
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return &media.IOError{Op: "marshal", Path: s.path, Reason: err.Error()}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return &media.IOError{Op: "write", Path: tmp, Reason: err.Error()}
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return &media.IOError{Op: "rename", Path: s.path, Reason: err.Error()}
	}
	return nil
}

// Load 读取检查点；文件不存在返回 ErrNoCheckpoint。
func (s *CheckpointStore) Load() (*Checkpoint, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoCheckpoint
		}
		return nil, &media.IOError{Op: "read", Path: s.path, Reason: err.Error()}
	}
	var cp Checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, &media.IOError{Op: "unmarshal", Path: s.path, Reason: err.Error()}
	}
	return &cp, nil
}

// ErrNoCheckpoint 表示尚无检查点，需要从头开始。
var ErrNoCheckpoint = errors.New("no checkpoint")

// Compatible 判断检查点是否仍匹配当前输入与参数；任一摘要不符即作废。
func (c *Checkpoint) Compatible(inputDigest, paramDigest string) bool {
	return c != nil && c.InputDigest == inputDigest && c.ParamDigest == paramDigest
}

// Remove 删除失效检查点（文件不存在视为成功）。
func (s *CheckpointStore) Remove() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return &media.IOError{Op: "remove", Path: s.path, Reason: err.Error()}
	}
	return nil
}
