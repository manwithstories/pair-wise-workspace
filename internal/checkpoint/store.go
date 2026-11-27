// Package checkpoint 管理流水线可恢复状态的持久化，
// 所有写入采用临时文件 + 原子改名，保证中断不会留下半份状态。
package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"bcastqc/internal/model"
)

// Stage 标识流水线执行阶段，用于断点续跑时跳过已完成步骤。
type Stage string

const (
	StageIngest   Stage = "ingest"
	StageAnalyze  Stage = "analyze"
	StageGraph    Stage = "graph"
	StageRepack   Stage = "repack"
	StageComplete Stage = "complete"
)

// State 是跨进程持久化的检查点内容。
type State struct {
	// JobID 标识一次演示任务，复跑时校验任务一致性。
	JobID string `json:"job_id"`
	// Stage 为已完成的最后阶段。
	Stage Stage `json:"stage"`
	// Steps 为各阶段已完成的单调步数，支持中断后精确续跑。
	Steps map[string]int64 `json:"steps"`
	// TrackSamples 记录每条轨道已确认的采样总数，续跑后必须保持不变。
	TrackSamples map[int]int64 `json:"track_samples"`
	// RecoveredSamples 为已写出的母版采样帧数。
	RecoveredSamples int64 `json:"recovered_samples"`
	// OutputSHA 为上一轮完整产物的 SHA-256，用于重复执行一致性核验。
	OutputSHA string `json:"output_sha"`
	// RunCount 记录完整跑完的次数（首次 1，续跑递增）。
	RunCount int `json:"run_count"`
}

// Store 封装检查点文件的读写路径。
type Store struct {
	path string
}

// NewStore 在给定目录下准备检查点存储。
func NewStore(dir string) *Store {
	return &Store{path: filepath.Join(dir, "checkpoint.json")}
}

// Path 返回检查点文件的最终路径。
func (s *Store) Path() string { return s.path }

// Exists 判断检查点是否已存在。
func (s *Store) Exists() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

// Load 读取并反序列化检查点；文件不存在时返回零值状态与 ok=false。
func (s *Store) Load() (*State, bool, error) {
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s.fresh(), false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: 读取检查点失败: %v", model.ErrCheckpoint, err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, false, fmt.Errorf("%w: 检查点内容损坏: %v", model.ErrCheckpoint, err)
	}
	if st.Steps == nil {
		st.Steps = make(map[string]int64)
	}
	if st.TrackSamples == nil {
		st.TrackSamples = make(map[int]int64)
	}
	return &st, true, nil
}

// Save 以原子替换方式写入检查点：先写同目录临时文件并 fsync，再 rename。
func (s *Store) Save(st *State) error {
	if st == nil {
		return fmt.Errorf("%w: 不能写入空状态", model.ErrCheckpoint)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("%w: 创建检查点目录失败: %v", model.ErrCheckpoint, err)
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: 序列化检查点失败: %v", model.ErrCheckpoint, err)
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("%w: 创建临时检查点失败: %v", model.ErrCheckpoint, err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("%w: 写入临时检查点失败: %v", model.ErrCheckpoint, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("%w: fsync 检查点失败: %v", model.ErrCheckpoint, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%w: 关闭临时检查点失败: %v", model.ErrCheckpoint, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%w: 原子替换检查点失败: %v", model.ErrCheckpoint, err)
	}
	return nil
}

// Reset 删除检查点，供全新任务使用。
func (s *Store) Reset() error {
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w: 删除旧检查点失败: %v", model.ErrCheckpoint, err)
	}
	return nil
}

// fresh 返回初始化好的空状态。
func (s *Store) fresh() *State {
	return &State{Steps: make(map[string]int64), TrackSamples: make(map[int]int64)}
}

// VerifySampleCounts 校验续跑时各轨道采样总数与检查点完全一致。
func VerifySampleCounts(st *State, current map[int]int64) error {
	if len(st.TrackSamples) == 0 {
		return nil
	}
	if len(st.TrackSamples) != len(current) {
		return fmt.Errorf("%w: 轨道数变化 检查点=%d 当前=%d",
			model.ErrUnrecoverable, len(st.TrackSamples), len(current))
	}
	for id, want := range st.TrackSamples {
		got, ok := current[id]
		if !ok {
			return fmt.Errorf("%w: 续跑缺少轨道 %d", model.ErrUnrecoverable, id)
		}
		if got != want {
			return fmt.Errorf("%w: 轨道 %d 采样数变化 检查点=%d 当前=%d",
				model.ErrUnrecoverable, id, want, got)
		}
	}
	return nil
}
