// Package checkpoint 原子保存检查点与输入摘要；
// 中断后只重跑失效依赖，输入变更则拒绝复用旧检查点。
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Version 为处理版本，版本变更会使旧检查点失效。
const Version = "mediagraph/1"

// State 为检查点内容：绑定输入摘要、参数与处理版本。
type State struct {
	Version     string                     `json:"version"`
	InputDigest string                     `json:"input_digest"`
	Params      map[string]string          `json:"params"`
	Completed   []string                   `json:"completed"` // 已完成步骤（有序）
	Results     map[string]json.RawMessage `json:"results"`   // 步骤 ID → 结果
}

// New 创建空检查点状态。
func New(inputDigest string, params map[string]string) *State {
	return &State{
		Version:     Version,
		InputDigest: inputDigest,
		Params:      params,
		Results:     map[string]json.RawMessage{},
	}
}

// MarkDone 记录步骤完成及其结果。
func (s *State) MarkDone(step string, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	for _, c := range s.Completed {
		if c == step {
			s.Results[step] = raw
			return nil
		}
	}
	s.Completed = append(s.Completed, step)
	s.Results[step] = raw
	return nil
}

// IsDone 报告步骤是否已完成。
func (s *State) IsDone(step string) bool {
	for _, c := range s.Completed {
		if c == step {
			return true
		}
	}
	return false
}

// Get 读取已完成步骤的结果。
func (s *State) Get(step string, out any) bool {
	raw, ok := s.Results[step]
	if !ok {
		return false
	}
	return json.Unmarshal(raw, out) == nil
}

// ErrInputChanged 表示输入摘要与检查点不一致。
var ErrInputChanged = errors.New("input digest changed, refusing to reuse checkpoint")

// ErrVersionChanged 表示处理版本不一致。
var ErrVersionChanged = errors.New("process version changed, refusing to reuse checkpoint")

// Validate 校验检查点是否可复用。
func (s *State) Validate(inputDigest string, params map[string]string) error {
	if s.Version != Version {
		return fmt.Errorf("%w: %s != %s", ErrVersionChanged, s.Version, Version)
	}
	if s.InputDigest != inputDigest {
		return fmt.Errorf("%w: %s != %s", ErrInputChanged, s.InputDigest, inputDigest)
	}
	for k, v := range params {
		if s.Params[k] != v {
			return fmt.Errorf("%w: param %s changed", ErrInputChanged, k)
		}
	}
	return nil
}

// DigestInputs 计算输入文件集合的联合摘要（按路径排序，含路径与长度）。
func DigestInputs(dir string, names []string) (string, error) {
	h := sha256.New()
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	for _, name := range sorted {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "|%s:%d\n", name, n)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

const fileName = "checkpoint.json"

// Save 原子写入检查点：临时文件 + fsync + rename。
func Save(dir string, s *State) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, fileName+".tmp")
	final := filepath.Join(dir, fileName)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// Load 读取检查点；不存在时返回 nil, nil。
func Load(dir string) (*State, error) {
	raw, err := os.ReadFile(filepath.Join(dir, fileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("corrupt checkpoint: %w", err)
	}
	return &s, nil
}
