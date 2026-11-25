package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"mediaarchive/internal/fault"
	"mediaarchive/internal/timeline"
)

// CheckpointVersion 是检查点格式版本。
const CheckpointVersion = 1

// Checkpoint 原子持久化输入游标、状态哈希、参数哈希和可恢复状态。
type Checkpoint struct {
	Version     int            `json:"version"`
	InputCursor int            `json:"input_cursor"`
	StateHash   string         `json:"state_hash"`
	ParamsHash  string         `json:"params_hash"`
	State       timeline.State `json:"state"`
	Interrupted bool           `json:"interrupted,omitempty"`
}

// HashJSON 计算任意可 JSON 序列化对象的稳定哈希。
func HashJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// StateHash 计算时间线状态哈希。
func StateHash(s timeline.State) (string, error) {
	return HashJSON(s)
}

// ParamsHash 计算配置参数哈希。
func ParamsHash(params any) (string, error) {
	return HashJSON(params)
}

// NewCheckpoint 组装并填充状态哈希。
func NewCheckpoint(cursor int, paramsHash string, state timeline.State, interrupted bool) (*Checkpoint, error) {
	h, err := StateHash(state)
	if err != nil {
		return nil, err
	}
	return &Checkpoint{
		Version:     CheckpointVersion,
		InputCursor: cursor,
		StateHash:   h,
		ParamsHash:  paramsHash,
		State:       state,
		Interrupted: interrupted,
	}, nil
}

// SaveCheckpoint 原子写入 checkpoint.json：同目录临时文件、sync、rename。
func SaveCheckpoint(path string, cp *Checkpoint) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fault.Wrap(fault.KindCheckpoint, "创建检查点目录失败", &fault.Location{Source: path}, err)
	}
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fault.Wrap(fault.KindCheckpoint, "序列化检查点失败", &fault.Location{Source: path}, err)
	}
	return AtomicWriteFile(path, b, 0o644)
}

// LoadCheckpoint 读取并校验状态哈希和参数哈希。
func LoadCheckpoint(path, expectedParamsHash string) (*Checkpoint, *fault.Error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fault.Wrap(fault.KindCheckpoint, "检查点不存在", &fault.Location{Source: path}, err)
		}
		return nil, fault.Wrap(fault.KindCheckpoint, "读取检查点失败", &fault.Location{Source: path}, err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, fault.Wrap(fault.KindCheckpoint, "检查点 JSON 损坏", &fault.Location{Source: path}, err)
	}
	got, err := StateHash(cp.State)
	if err != nil {
		return nil, fault.Wrap(fault.KindIntegrity, "检查点状态哈希计算失败", &fault.Location{Source: path}, err)
	}
	if got != cp.StateHash {
		return nil, fault.New(fault.KindIntegrity, "检查点状态哈希不一致", &fault.Location{Source: path})
	}
	if expectedParamsHash != "" && cp.ParamsHash != expectedParamsHash {
		return nil, fault.New(fault.KindIntegrity, "参数哈希与检查点不匹配", &fault.Location{Source: path})
	}
	if cp.InputCursor != cp.State.Cursor {
		return nil, fault.New(fault.KindIntegrity, "检查点游标与状态游标不一致", &fault.Location{Source: path})
	}
	return &cp, nil
}

// AtomicWriteFile 在同一目录写入临时文件后 rename，保证目标文件要么是旧内容要么是完整新内容。
func AtomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
