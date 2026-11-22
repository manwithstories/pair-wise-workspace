// Package checkpoint 管理流水线检查点的原子读写与恢复。
//
// 推进检查点前，片段结果与清单必须已经落盘；状态文件本身通过
// “临时文件 + 原子替换”更新，因此重启时要么看到完整旧状态，要么
// 看到完整新状态，不会读到半写内容。输入摘要变化会使相关片段失效。
package checkpoint

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"mediad/internal/fault"
	"mediad/internal/model"
)

const stateFileName = "checkpoint.json"

// State 是检查点持久化内容。
type State struct {
	Version  int             `json:"version"`
	InputID  string          `json:"input_id"`
	Segments []model.Segment `json:"segments"`
	// Done 记录已完成片段 ID，保证重启后不重复计数。
	Done map[string]bool `json:"done"`
}

// Checkpoint 绑定一个状态目录。
type Checkpoint struct {
	path string
}

// New 在 dir 下打开检查点存储。
func New(dir string) (*Checkpoint, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fault.Fatal(fault.CodeInvalidInput, "checkpoint.MkdirAll", err)
	}
	return &Checkpoint{path: filepath.Join(dir, stateFileName)}, nil
}

// Load 读取检查点；文件不存在返回空状态与 nil 错误。
func (c *Checkpoint) Load() (*State, error) {
	raw, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return &State{Done: map[string]bool{}}, nil
	}
	if err != nil {
		return nil, fault.Fatal(fault.CodeInvalidInput, "checkpoint.Load", err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fault.Recoverable(fault.CodeCorrupt,
			"checkpoint.Unmarshal", 0, 0, err)
	}
	if st.Done == nil {
		st.Done = map[string]bool{}
	}
	return &st, nil
}

// Advance 在片段结果与清单已落盘的前提下原子推进检查点。
// 同一输入重复推进相同片段是幂等的：Done 集合不会重复计数。
func (c *Checkpoint) Advance(inputID string, segs []model.Segment) (*State, error) {
	st, err := c.Load()
	if err != nil {
		return nil, err
	}
	// 输入变化：整个状态失效，旧片段结果不再可信。
	if st.InputID != "" && st.InputID != inputID {
		st = &State{Version: 1, InputID: inputID, Done: map[string]bool{}}
	}
	if st.InputID == "" {
		st.InputID = inputID
		st.Version = 1
	}
	for _, sg := range segs {
		st.Segments = mergeSegment(st.Segments, sg)
		st.Done[sg.ID] = true
	}
	if err := c.writeAtomic(st); err != nil {
		return nil, err
	}
	return st, nil
}

// InvalidateInput 在输入摘要变化时显式使全部片段失效。
func (c *Checkpoint) InvalidateInput() error {
	st, err := c.Load()
	if err != nil {
		return err
	}
	reset := &State{Version: st.Version, InputID: "", Done: map[string]bool{}}
	return c.writeAtomic(reset)
}

// IsDone 报告片段是否已在当前输入下完成。
func (c *Checkpoint) IsDone(inputID, segID string) (bool, error) {
	st, err := c.Load()
	if err != nil {
		return false, err
	}
	if st.InputID != inputID {
		return false, nil
	}
	return st.Done[segID], nil
}

func mergeSegment(existing []model.Segment, sg model.Segment) []model.Segment {
	for i := range existing {
		if existing[i].ID == sg.ID {
			existing[i] = sg
			return existing
		}
	}
	return append(existing, sg)
}

func (c *Checkpoint) writeAtomic(st *State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fault.Fatal(fault.CodeInvalidInput, "checkpoint.Marshal", err)
	}
	dir := filepath.Dir(c.path)
	tmp, err := os.CreateTemp(dir, ".ckpt-*")
	if err != nil {
		return fault.Fatal(fault.CodeInvalidInput, "checkpoint.Temp", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fault.Fatal(fault.CodeInvalidInput, "checkpoint.Write", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fault.Fatal(fault.CodeInvalidInput, "checkpoint.Sync", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fault.Fatal(fault.CodeInvalidInput, "checkpoint.Close", err)
	}
	if err := os.Rename(tmpName, c.path); err != nil {
		cleanup()
		return fault.Fatal(fault.CodeInvalidInput, "checkpoint.Rename", err)
	}
	return nil
}
