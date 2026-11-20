// Package checkpoint 把分块扫描状态与校验和原子写入本地索引。
// 恢复时先验证输入摘要与版本，仅复用校验通过的块；损坏或版本不匹配
// 则整体回退重扫，杜绝复用脏状态。
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

	"media-audit/internal/stream"
)

// SchemaVersion 是检查点结构版本，结构演进时递增以触发回退。
const SchemaVersion = 1

// TrackState 是单条轨道（表示层）的检查点状态。
type TrackState struct {
	TrackID     int                  `json:"track_id"`
	Codec       string               `json:"codec"`
	InputSize   int64                `json:"input_size"`
	InputSHA256 string               `json:"input_sha256"`
	Completed   []stream.BlockRecord `json:"completed_blocks"`
}

// Store 是落盘的完整检查点。
type Store struct {
	SchemaVersion int          `json:"schema_version"`
	JobID         string       `json:"job_id"`
	Tracks        []TrackState `json:"tracks"`
	OutputSHA256  string       `json:"output_sha256"` // 已提交输出的校验和
	SHA256        string       `json:"sha256"`        // 自身内容校验和
}

// LoadOutcome 描述恢复时的检查点判定。
type LoadOutcome string

// 恢复判定结果。
const (
	OutcomeReusable LoadOutcome = "reusable" // 全部块可复用
	OutcomePartial  LoadOutcome = "partial"  // 部分块校验通过，可续扫
	OutcomeReset    LoadOutcome = "reset"    // 版本/摘要不匹配，整体回退
)

// LoadResult 携带恢复判定与可安全复用的块数。
type LoadResult struct {
	Outcome      LoadOutcome
	Reusable     map[int][]stream.BlockRecord // trackID -> 已验证块
	Store        *Store
	ResetReason  string
}

// path 构造检查点文件路径。
func path(dir, jobID string) string {
	return filepath.Join(dir, "checkpoint-"+sanitize(jobID)+".json")
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

// Save 原子写入检查点（临时文件 + fsync + 回读校验 + 重命名）。
func Save(dir string, st *Store) error {
	st.SchemaVersion = SchemaVersion
	st.SHA256 = ""
	st.SHA256 = digest(st)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建检查点目录失败: %w", err)
	}
	payload, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	full := path(dir, st.JobID)
	tmp, err := os.CreateTemp(dir, ".ckpt-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时检查点失败: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := verifyBytes(tmpName, payload); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, full); err != nil {
		cleanup()
		return fmt.Errorf("检查点原子替换失败: %w", err)
	}
	return nil
}

// Load 读取检查点并按当前输入摘要验证。expected 为各轨道当前的
// 输入大小与哈希；任何不匹配都导致该轨道回退，版本不符则整体回退。
func Load(dir, jobID string, expected map[int]InputRef) (*LoadResult, error) {
	raw, err := os.ReadFile(path(dir, jobID))
	if errors.Is(err, os.ErrNotExist) {
		return &LoadResult{Outcome: OutcomeReset, ResetReason: "检查点不存在", Reusable: map[int][]stream.BlockRecord{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var st Store
	if err := json.Unmarshal(raw, &st); err != nil {
		return &LoadResult{Outcome: OutcomeReset, ResetReason: "检查点 JSON 损坏", Reusable: map[int][]stream.BlockRecord{}}, nil
	}
	if st.SchemaVersion != SchemaVersion {
		return &LoadResult{
			Outcome: OutcomeReset, ResetReason: "检查点版本不匹配",
			Reusable: map[int][]stream.BlockRecord{}, Store: &st,
		}, nil
	}
	// 自校验：内容哈希不符说明检查点本身被破坏。
	want := digest(&st)
	if st.SHA256 != want {
		return &LoadResult{
			Outcome: OutcomeReset, ResetReason: "检查点内容校验不一致",
			Reusable: map[int][]stream.BlockRecord{}, Store: &st,
		}, nil
	}

	res := &LoadResult{Outcome: OutcomeReusable, Reusable: map[int][]stream.BlockRecord{}, Store: &st}
	for _, tr := range st.Tracks {
		ref, ok := expected[tr.TrackID]
		if !ok || ref.Size != tr.InputSize || ref.SHA256 != tr.InputSHA256 {
			res.Outcome = OutcomePartial
			continue // 该轨道输入已变，丢弃其全部块
		}
		res.Reusable[tr.TrackID] = append([]stream.BlockRecord(nil), tr.Completed...)
	}
	if len(res.Reusable) < len(st.Tracks) {
		res.Outcome = OutcomePartial
	}
	return res, nil
}

// InputRef 是恢复时用于核对的当前输入引用。
type InputRef struct {
	Size   int64
	SHA256 string
}

// VerifyBlocks 用当前输入的实际块哈希验证候选块，仅返回校验通过者。
// blockHashes 为按块号查当前输入重算哈希的函数，便于注入磁盘故障测试。
func VerifyBlocks(candidates []stream.BlockRecord, blockHashes func(number int) (string, error)) []stream.BlockRecord {
	var good []stream.BlockRecord
	for _, blk := range candidates {
		got, err := blockHashes(blk.Number)
		if err != nil || got != blk.SHA256 {
			continue // 块损坏：跳过，触发该块重扫
		}
		good = append(good, blk)
	}
	sort.Slice(good, func(i, j int) bool { return good[i].Number < good[j].Number })
	return good
}

// Delete 移除检查点（用于不可恢复错误后的清理）。
func Delete(dir, jobID string) error {
	err := os.Remove(path(dir, jobID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func digest(st *Store) string {
	clone := *st
	clone.SHA256 = ""
	raw, _ := json.Marshal(&clone)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func verifyBytes(path string, want []byte) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if sha256.Sum256(got) != sha256.Sum256(want) {
		return errors.New("检查点回读内容不一致")
	}
	return nil
}
