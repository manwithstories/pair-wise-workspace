// Package checkpoint 原子保存流水线检查点。
// 保存失败时既有检查点不被破坏。
package checkpoint

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"
)

// Checkpoint 记录一个处理阶段的输入/输出摘要与来源。
type Checkpoint struct {
	Stage        string    `json:"stage"`
	InputDigest  string    `json:"input_digest"`
	OutputDigest string    `json:"output_digest"`
	Source       string    `json:"source"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Store 管理磁盘上的检查点文件。
type Store struct {
	Path string
}

// Save 原子写入检查点：先写临时文件再重命名。
func (s *Store) Save(cp Checkpoint) error {
	cp.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

// Load 读取检查点；不存在时返回 ok=false。
func (s *Store) Load() (cp Checkpoint, ok bool, err error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return Checkpoint{}, false, nil
	}
	if err != nil {
		return Checkpoint{}, false, err
	}
	if err := json.Unmarshal(data, &cp); err != nil {
		return Checkpoint{}, false, err
	}
	return cp, true, nil
}
