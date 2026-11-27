// Package resume 实现分段检查点与中断续跑。检查点以原子替换落盘
// （临时文件 + fsync + rename），并绑定源素材哈希；续跑时只重放未提交段，
// 已提交段通过记录的块哈希与文件偏移做完整性核验，保证两条执行路径产物一致。
package resume

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"bcast/internal/fault"
)

// SchemaVersion 是检查点格式版本，格式不兼容演进时递增。
const SchemaVersion = 1

// Checkpoint 是单个已提交段的不可变记录。
type Checkpoint struct {
	Segment     int    `json:"segment"`      // 段序号（主时钟网格）
	SampleCount int    `json:"sample_count"` // 该段样本数
	EndOffset   int64  `json:"end_offset"`   // 提交后输出文件的字节长度
	BlockSHA256 string `json:"block_sha256"` // 该段块体的像素/字节哈希
}

// Journal 是一次运行的完整检查点状态。JSON 字段顺序固定、不含 map，
// 同输入多次运行序列化结果逐字节一致。
type Journal struct {
	SchemaVersion int          `json:"schema_version"`
	SourceSHA256  string       `json:"source_sha256"` // 绑定的源素材哈希
	SegmentUS     int64        `json:"segment_us"`    // 段网格（微秒）
	Committed     []Checkpoint `json:"committed"`     // 严格按段序号连续递增
}

// ErrNoJournal 表示尚无检查点（首次运行）。
var ErrNoJournal = errors.New("resume: journal not found")

// Load 读取并校验检查点；文件不存在返回 ErrNoJournal。
func Load(path string) (*Journal, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoJournal
	}
	if err != nil {
		return nil, err
	}
	var j Journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fault.New(fault.ErrInvalidInput, fault.KindResume, 0, 0, 0, 0,
			"corrupt journal: "+err.Error())
	}
	if j.SchemaVersion != SchemaVersion {
		return nil, fault.InvalidInputf("journal schema version %d, want %d",
			j.SchemaVersion, SchemaVersion)
	}
	if err := j.validate(); err != nil {
		return nil, err
	}
	return &j, nil
}

// validate 检查提交记录的连续性与单调偏移，任何断裂都视为不可信检查点。
func (j *Journal) validate() error {
	if j.SchemaVersion != SchemaVersion {
		return fault.InvalidInputf("journal schema version %d, want %d",
			j.SchemaVersion, SchemaVersion)
	}
	for i, c := range j.Committed {
		if c.Segment != i {
			return fault.New(fault.ErrBoundary, fault.KindResume, 0, uint64(c.Segment),
				0, c.EndOffset, "non-contiguous checkpoint segment")
		}
		if i > 0 && c.EndOffset <= j.Committed[i-1].EndOffset {
			return fault.New(fault.ErrBoundary, fault.KindResume, 0, uint64(c.Segment),
				0, c.EndOffset, "checkpoint offset must advance")
		}
		if len(c.BlockSHA256) != 64 {
			return fault.InvalidInputf("checkpoint %d block hash must be 64 hex chars", i)
		}
	}
	return nil
}

// Save 以“临时文件 + fsync + 原子 rename”落盘，杜绝半写检查点。
func (j *Journal) Save(path string) error {
	if err := j.validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".journal-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil { // fsync 保证崩溃后可见
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// LastCommitted 返回最后一个已提交段序号；无提交时返回 -1。
func (j *Journal) LastCommitted() int {
	if len(j.Committed) == 0 {
		return -1
	}
	return j.Committed[len(j.Committed)-1].Segment
}

// BindSource 校验检查点绑定的源素材哈希与当前输入一致，防止跨输入误用。
func (j *Journal) BindSource(sourceSHA256 string) error {
	if j.SourceSHA256 != sourceSHA256 {
		return fault.New(fault.ErrChecksum, fault.KindResume, 0, 0, 0, 0,
			fmt.Sprintf("journal bound to %s but input is %s", j.SourceSHA256, sourceSHA256))
	}
	return nil
}

// Commit 追加一条检查点并立即原子落盘。
func (j *Journal) Commit(path string, c Checkpoint) error {
	if c.Segment != len(j.Committed) {
		return fault.New(fault.ErrBoundary, fault.KindResume, 0, uint64(c.Segment),
			0, c.EndOffset, "commit out of order")
	}
	j.Committed = append(j.Committed, c)
	return j.Save(path)
}
