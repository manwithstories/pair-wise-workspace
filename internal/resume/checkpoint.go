// Package resume 负责检查点的原子写入、输入身份核对与续跑校验。
// 恢复时核对源块哈希、参数摘要与已提交分段校验和；任一不匹配即拒绝续跑，
// 并指出首个不一致位置，保证重复运行产物确定。
package resume

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"media-pipeline/internal/fault"
)

// SourceRef 标识一个输入源块及其内容哈希。
type SourceRef struct {
	BlockID string `json:"block_id"`
	Sha256  string `json:"sha256"` // 64 位十六进制
}

// SegmentRef 记录一个已提交分段的序号、采样范围与内容校验和。
type SegmentRef struct {
	Index       uint64 `json:"index"`
	FirstSample uint64 `json:"first_sample"`
	EndSample   uint64 `json:"end_sample"`
	Sha256      string `json:"sha256"`
}

// Checkpoint 是可复核的断点状态。
type Checkpoint struct {
	Version     int          `json:"version"`
	ParamDigest string       `json:"param_digest"` // 参数摘要
	Sources     []SourceRef  `json:"sources"`      // 已消费源块（有序）
	Segments    []SegmentRef `json:"segments"`     // 已提交分段（有序）
	NextSample  uint64       `json:"next_sample"`  // 续跑起点采样序号
	SelfSha256  string       `json:"self_sha256"`  // 除本字段外内容的哈希
}

// CurrentVersion 是检查点格式版本。
const CurrentVersion = 1

const checkpointName = "checkpoint.json"

// Path 返回给定输出目录下的检查点路径。
func Path(dir string) string { return filepath.Join(dir, checkpointName) }

// contentHash 计算检查点除 SelfSha256 字段外的规范 JSON 哈希。
func contentHash(c Checkpoint) [32]byte {
	cp := c
	cp.SelfSha256 = ""
	raw, _ := json.Marshal(cp)
	return sha256.Sum256(raw)
}

// Write 先写临时文件再原子改名，避免进程中断留下半截检查点。
func Write(dir string, c Checkpoint) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fault.New(fault.KindOther, "", -1, -1, false,
			"cannot create checkpoint dir").WithOp("resume.write").Wrap(err)
	}
	c.Version = CurrentVersion
	sum := contentHash(c)
	c.SelfSha256 = hex.EncodeToString(sum[:])

	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fault.New(fault.KindOther, "", -1, -1, false,
			"checkpoint marshal failed").WithOp("resume.write").Wrap(err)
	}
	final := Path(dir)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fault.New(fault.KindOther, "", -1, -1, false,
			"temp checkpoint write failed").WithOp("resume.write").Wrap(err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fault.New(fault.KindOther, "", -1, -1, false,
			"checkpoint atomic rename failed").WithOp("resume.write").Wrap(err)
	}
	return nil
}

// Read 读取并校验检查点自哈希；损坏或被篡改即报不可恢复冲突。
func Read(dir string) (*Checkpoint, error) {
	raw, err := os.ReadFile(Path(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // 无检查点意味着首次运行
		}
		return nil, fault.New(fault.KindOther, "", -1, -1, false,
			"checkpoint read failed").WithOp("resume.read").Wrap(err)
	}
	var c Checkpoint
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, conflict(-1, "checkpoint is not valid JSON").Wrap(err)
	}
	wantSum := contentHash(c)
	want := hex.EncodeToString(wantSum[:])
	if c.SelfSha256 != want {
		return nil, conflict(-1, "checkpoint self-checksum mismatch: tampered or corrupted")
	}
	return &c, nil
}

// Verify 核对当前输入身份与参数摘要是否与检查点一致。
// 返回首个不一致位置（源块下标或 -1 表示参数/长度层面）。
func Verify(c *Checkpoint, paramDigest string, sources []SourceRef) (int, error) {
	if c.ParamDigest != paramDigest {
		return -1, conflict(-1, fmt.Sprintf(
			"parameter digest mismatch: checkpoint=%s current=%s", c.ParamDigest, paramDigest))
	}
	if len(c.Sources) > len(sources) {
		return len(sources), conflict(int64(len(sources)),
			"checkpoint records more sources than the current input provides")
	}
	for i, want := range c.Sources {
		got := sources[i]
		if want.BlockID != got.BlockID || want.Sha256 != got.Sha256 {
			return i, conflict(int64(i), fmt.Sprintf(
				"source mismatch at index %d: checkpoint=(%s,%s) current=(%s,%s)",
				i, want.BlockID, want.Sha256, got.BlockID, got.Sha256))
		}
	}
	return -1, nil
}

// VerifySegment 核对单个已提交分段的校验和，返回首个冲突的分段下标。
func VerifySegment(c *Checkpoint, idx uint64, sum [32]byte) (int, error) {
	wantHex := hex.EncodeToString(sum[:])
	for i, seg := range c.Segments {
		if seg.Index != idx {
			continue
		}
		if seg.Sha256 != wantHex {
			return i, conflict(int64(i), fmt.Sprintf(
				"segment %d checksum mismatch: checkpoint=%s current=%s",
				idx, seg.Sha256, wantHex))
		}
		return i, nil
	}
	return -1, conflict(int64(idx), fmt.Sprintf("segment %d missing from checkpoint", idx))
}

// Equal 比较两个检查点的可复核内容是否完全一致（用于确定性自检）。
func Equal(a, b *Checkpoint) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return bytes.Equal(ra, rb)
}

func conflict(sampleOff int64, msg string) *fault.Error {
	return fault.New(fault.KindCheckpointConflict, "", -1, sampleOff, false,
		msg).WithOp("resume.verify")
}
