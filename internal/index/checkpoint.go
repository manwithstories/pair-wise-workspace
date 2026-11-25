// Package index 负责本地索引与检查点的原子写入、校验和验证及断点续跑。
//
// 每处理 N 个分片落盘一个检查点：记录输入摘要、配置摘要、输出校验和与已完成序号。
// 校验和不符或文件截断时回退到上一个有效块；输入或配置变化时拒绝续跑。
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrRejectResume 表示输入或配置摘要与检查点不一致，拒绝续跑。
var ErrRejectResume = errors.New("resume rejected: input or config changed")

// ErrCheckpointCorrupt 表示检查点截断或校验和不符。
var ErrCheckpointCorrupt = errors.New("checkpoint corrupt")

// Checkpoint 是单个块边界的可持久化状态。
type Checkpoint struct {
	Version     int      `json:"version"`
	Chunk       int      `json:"chunk"`        // 块大小 N
	NextSeq     int      `json:"next_seq"`     // 下一个待处理分片序号
	DoneSeqs    []int    `json:"done_seqs"`    // 已完成分片序号（有序）
	InputDigest string   `json:"input_digest"` // 输入摘要
	ConfigDigest string  `json:"config_digest"`// 配置摘要
	OutputSHA   string   `json:"output_sha"`   // 已累积输出的校验和
	UpdatedAt   int64    `json:"updated_at"`   // Unix 纳秒时间戳
}

// checkpointFile 是磁盘封装：Body + Body 的 SHA256（hex）。
type checkpointFile struct {
	Body []byte `json:"body"`
	SHA  string `json:"sha256"`
}

const currentVersion = 1

func checkpointPath(base string, chunkID int) string {
	return filepath.Join(base, fmt.Sprintf("checkpoint.%d.json", chunkID))
}

// WriteAtomic 先写临时文件再 rename，保证检查点要么完整出现要么不存在。
func WriteAtomic(path string, cp Checkpoint) error {
	body, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	wrapped := checkpointFile{Body: body, SHA: hex.EncodeToString(sum[:])}
	data, err := json.MarshalIndent(wrapped, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadValid 读取并验证检查点；截断或校验和不符返回 ErrCheckpointCorrupt。
func ReadValid(path string) (Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Checkpoint{}, err
	}
	var wrapped checkpointFile
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return Checkpoint{}, fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	sum := sha256.Sum256(wrapped.Body)
	if hex.EncodeToString(sum[:]) != wrapped.SHA {
		return Checkpoint{}, fmt.Errorf("%w: checksum mismatch", ErrCheckpointCorrupt)
	}
	var cp Checkpoint
	if err := json.Unmarshal(wrapped.Body, &cp); err != nil {
		return Checkpoint{}, fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	if cp.Version != currentVersion {
		return Checkpoint{}, fmt.Errorf("%w: unsupported version %d", ErrCheckpointCorrupt, cp.Version)
	}
	return cp, nil
}
