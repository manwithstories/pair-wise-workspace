// Package manifest 写出交付复核清单：逐帧哈希、质量统计、块来源与
// 检查点摘要；输入损坏或同序号冲突时拒绝生成最终清单。
package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"

	"medpipe/internal/clock"
	"medpipe/internal/quality"
	"medpipe/internal/timeline"
)

// Manifest 是最终交付清单的稳定 JSON 结构。
type Manifest struct {
	Schema   string             `json:"schema"`
	Seed     int64              `json:"seed"`
	Rate     int64              `json:"sample_rate"`
	Channels int                `json:"channels"`
	FPS      string             `json:"fps"`
	Fault    string             `json:"fault"`
	Summary  quality.Summary    `json:"summary"`
	Frames   []quality.FrameMetric `json:"frames"`
	Sources  []timeline.SourceRange `json:"sources"`
	Blocks   []BlockRecord      `json:"blocks"`
	Checkpoint string           `json:"checkpoint_sha256"`
}

// BlockRecord 记录每个输入块的扫描与补偿证据。
type BlockRecord struct {
	ID              int64  `json:"id"`
	SHA256          string `json:"sha256"`
	Frames          int64  `json:"frames"`
	MissingFrames   int64  `json:"missing_frames"`
	Truncated       bool   `json:"truncated"`
	Compensated     int64  `json:"compensated_samples"`
	TimeAlignError  int64  `json:"time_align_error_samples"`
}

// Build 组装清单；调用方须先确认没有 CodeInvalidHeader/CodeConflict。
func Build(seed int64, rate int64, channels int, fps clock.Rational, fault string,
	frames []quality.FrameMetric, sum quality.Summary,
	sources []timeline.SourceRange, blocks []BlockRecord, checkpointSHA string) *Manifest {
	return &Manifest{
		Schema:     "medpipe/manifest/v1",
		Seed:       seed,
		Rate:       rate,
		Channels:   channels,
		FPS:        fps.String(),
		Fault:      fault,
		Summary:    sum,
		Frames:     frames,
		Sources:    sources,
		Blocks:     blocks,
		Checkpoint: checkpointSHA,
	}
}

// Write 落盘清单；若目标目录不可写返回检查点类错误。
func (m *Manifest) Write(dir string) (string, error) {
	if m == nil {
		return "", clock.NewError(clock.CodeConflict, -1, "manifest: 拒绝为空结果生成清单")
	}
	// 损坏/冲突结果在流水线层已拦截，这里再做一次不变量防御。
	if m.Summary.MaxErrorSamples > 1 {
		return "", clock.NewError(clock.CodeInvalidHeader, -1,
			"manifest: 时间对齐误差 %d 超 1 采样，拒绝清单", m.Summary.MaxErrorSamples)
	}
	path, err := clockSafeJoin(dir, "manifest.json")
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", clock.WrapError(clock.CodeCheckpoint, -1, err, "清单序列化失败")
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", clock.WrapError(clock.CodeCheckpoint, -1, err, "清单写入失败")
	}
	return path, nil
}

// Load 读取并解析清单，供 verify/resume 子命令使用。
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, clock.WrapError(clock.CodeCheckpoint, -1, err, "清单读取失败")
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, clock.WrapError(clock.CodeCheckpoint, -1, err, "清单解析失败")
	}
	if m.Schema != "medpipe/manifest/v1" {
		return nil, clock.NewError(clock.CodeCheckpoint, -1, "清单 schema 不匹配: %s", m.Schema)
	}
	return &m, nil
}

// Verify 重新计算磁盘上每帧的哈希并与清单比对，任一不符即报错。
func (m *Manifest) Verify(dir string) error {
	if int64(len(m.Frames)) != m.Summary.Frames {
		return clock.NewError(clock.CodeChecksum, -1,
			"manifest: 帧数声明 %d 与实际记录 %d 不符", m.Summary.Frames, len(m.Frames))
	}
	for _, fm := range m.Frames {
		full, err := clockSafeJoin(dir, fm.File)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(full)
		if err != nil {
			return clock.WrapError(clock.CodeChecksum, fm.Index, err, "帧文件不可读")
		}
		if got := quality.HashBytes(raw); got != fm.SHA256 {
			return clock.NewError(clock.CodeChecksum, fm.Index, "帧 %d 哈希不匹配", fm.Index)
		}
	}
	return nil
}

func clockSafeJoin(dir, name string) (string, error) {
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) || clean == ".." || filepath.IsAbs(name) {
		return "", clock.NewError(clock.CodePathRejected, -1, "manifest: 非法路径 %s", name)
	}
	return filepath.Join(dir, clean), nil
}
