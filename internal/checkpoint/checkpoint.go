// Package checkpoint 实现可复核的断点续跑。
// 检查点同时固化输入摘要、参数与已完成进度；续跑时重新计算输入摘要，
// 与检查点不匹配则拒绝续跑，防止用不同输入套用旧进度。
package checkpoint

import (
	"encoding/json"
	"errors"
	"os"
)

// State 是检查点的全部持久化字段，JSON 键名固定以保证可复核。
type State struct {
	Version      int    `json:"version"`
	InputDigest  string `json:"input_digest"`
	ParamsDigest string `json:"params_digest"`
	FramesDone   int    `json:"frames_done"`
	Stage        string `json:"stage"`
	ManifestHash string `json:"manifest_hash,omitempty"`
}

// ErrDigestMismatch 表示检查点与当前输入摘要不匹配，拒绝续跑。
var ErrDigestMismatch = errors.New("checkpoint: 输入摘要与检查点不匹配，拒绝续跑")

// ErrNoCheckpoint 表示路径下不存在检查点，应按全新运行处理。
var ErrNoCheckpoint = errors.New("checkpoint: 检查点不存在")

// CurrentVersion 是检查点格式版本。
const CurrentVersion = 1

// Save 原子写入检查点：先写临时文件再改名，避免崩溃留下半截文件。
func Save(path string, s State) error {
	s.Version = CurrentVersion
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load 读取检查点；文件不存在返回 ErrNoCheckpoint。
func Load(path string) (State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, ErrNoCheckpoint
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, err
	}
	return s, nil
}

// Verify 校验检查点是否可用于当前输入；不匹配返回 ErrDigestMismatch。
func Verify(s State, inputDigest, paramsDigest string) error {
	if s.Version != CurrentVersion {
		return ErrDigestMismatch
	}
	if s.InputDigest != inputDigest || s.ParamsDigest != paramsDigest {
		return ErrDigestMismatch
	}
	return nil
}

// Remove 删除检查点文件，用于成功发布后的清理。
func Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
