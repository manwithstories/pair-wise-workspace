package index

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Store 管理一个索引目录下的检查点链与输出校验和。
type Store struct {
	dir          string
	chunk        int
	inputDigest  string
	configDigest string
	outHash      hash.Hash // 累积输出内容的运行哈希
	latestChunk  int         // 已成功写入的最大块 ID，-1 表示无
}

// NewStore 打开（或创建）索引目录。
func NewStore(dir string, chunk int, inputDigest, configDigest string) (*Store, error) {
	if chunk <= 0 {
		return nil, errors.New("chunk size must be positive")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		dir: dir, chunk: chunk,
		inputDigest: inputDigest, configDigest: configDigest,
		outHash:     newChainHash(),
		latestChunk: -1,
	}
	return s, nil
}

// AppendOutput 把一块已确定的输出纳入运行校验和（写入顺序确定，保证幂等）。
func (s *Store) AppendOutput(b []byte) {
	s.outHash.Write(b)
}

// OutputSHA 返回当前累积输出的十六进制校验和。
func (s *Store) OutputSHA() string {
	return hex.EncodeToString(s.outHash.Sum(nil))
}

// Save 在处理完一批分片后原子写入第 chunkID 个检查点。
func (s *Store) Save(chunkID, nextSeq int, doneSeqs []int) (Checkpoint, error) {
	cp := Checkpoint{
		Version: currentVersion, Chunk: s.chunk, NextSeq: nextSeq,
		DoneSeqs: append([]int(nil), doneSeqs...),
		InputDigest: s.inputDigest, ConfigDigest: s.configDigest,
		OutputSHA: s.OutputSHA(), UpdatedAt: time.Now().UnixNano(),
	}
	if err := WriteAtomic(checkpointPath(s.dir, chunkID), cp); err != nil {
		return Checkpoint{}, err
	}
	s.latestChunk = chunkID
	return cp, nil
}

// LatestValid 从最大块 ID 向前找第一个有效检查点；损坏块跳过（回退语义）。
// 返回该检查点与被跳过的损坏块 ID 列表；无任何检查点时返回零值与 false。
func (s *Store) LatestValid(maxChunk int) (Checkpoint, []int, bool) {
	var corrupt []int
	for id := maxChunk; id >= 0; id-- {
		cp, err := ReadValid(checkpointPath(s.dir, id))
		if err == nil {
			return cp, corrupt, true
		}
		if !errors.Is(err, os.ErrNotExist) {
			corrupt = append(corrupt, id)
		}
	}
	return Checkpoint{}, corrupt, false
}

// Resume 校验最近有效检查点是否匹配当前输入与配置；不匹配则拒绝续跑。
// 匹配时恢复输出校验和状态并返回检查点，使重复分析幂等。
func (s *Store) Resume(maxChunk int) (Checkpoint, error) {
	cp, corrupt, ok := s.LatestValid(maxChunk)
	if !ok {
		return Checkpoint{}, fmt.Errorf("%w: no usable checkpoint", ErrCheckpointCorrupt)
	}
	if cp.InputDigest != s.inputDigest || cp.ConfigDigest != s.configDigest {
		return Checkpoint{}, fmt.Errorf("%w (input=%t config=%t)",
			ErrRejectResume, cp.InputDigest != s.inputDigest, cp.ConfigDigest != s.configDigest)
	}
	// 用持久化的输出校验和重置运行哈希：写入空状态使其 Sum 等于历史值。
	s.outHash = resumeHash(cp.OutputSHA)
	s.latestChunk = maxChunk - len(corrupt)
	return cp, nil
}

// Dir 返回索引目录。
func (s *Store) Dir() string { return s.dir }

// DigestInputs 对输入目录的文件路径、大小与内容哈希求稳定摘要。
func DigestInputs(dir string) (string, error) {
	h := sha256.New()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		full := filepath.Join(dir, name)
		data, err := os.ReadFile(full)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s:%d:", name, len(data))
		fh := sha256.Sum256(data)
		h.Write(fh[:])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DigestConfig 对影响分析结果的配置项求稳定摘要。
func DigestConfig(chunk, queue int, mem int64, extra string) string {
	h := sha256.New()
	fmt.Fprintf(h, "chunk=%d queue=%d mem=%d extra=%s", chunk, queue, mem, extra)
	return hex.EncodeToString(h.Sum(nil))
}
