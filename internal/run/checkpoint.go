// Package run 实现流式执行：读取索引与 RTP 输入，经传输恢复、
// 音频处理、封装输出，并支持分段边界续跑。
package run

import (
	"encoding/json"
	"os"

	"mediaverify/internal/fault"
	"mediaverify/internal/scan"
)

// Checkpoint 记录已完成分段边界，供中断后续跑。
type Checkpoint struct {
	InputDigest       string `json:"input_digest"`
	CompletedSegments int    `json:"completed_segments"`
	OutputBytes       int64  `json:"output_bytes"`
	Frames            int64  `json:"frames"`
	Checksum          string `json:"checksum"`
}

func (c *Checkpoint) sum() string {
	b, _ := json.Marshal(struct {
		D string `json:"d"`
		S int    `json:"s"`
		B int64  `json:"b"`
		F int64  `json:"f"`
	}{c.InputDigest, c.CompletedSegments, c.OutputBytes, c.Frames})
	return scan.DigestBytes(b)
}

// SaveCheckpoint 原子写检查点。
func SaveCheckpoint(path string, c *Checkpoint) error {
	c.Checksum = c.sum()
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// LoadCheckpoint 读取并校验检查点：
// 摘要 mismatch、检查点截断、输出尾部不完整均视为不可信，
// 返回 Rescan 策略错误，调用方须丢弃状态并重新扫描。
func LoadCheckpoint(path, inputDigest, outputPath string) (*Checkpoint, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fault.New(fault.KindResume, fault.Rescan, path, 0, -1,
			"no checkpoint: %v", err)
	}
	var c Checkpoint
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fault.New(fault.KindResume, fault.Rescan, path, 0, -1,
			"checkpoint truncated or corrupt: %v", err)
	}
	if c.Checksum == "" || c.Checksum != c.sum() {
		return nil, fault.New(fault.KindResume, fault.Rescan, path, 0, -1,
			"checkpoint checksum mismatch")
	}
	if c.InputDigest != inputDigest {
		return nil, fault.New(fault.KindResume, fault.Rescan, path, 0, -1,
			"input digest mismatch: checkpoint not trustworthy")
	}
	// 输出尾部完整性：文件大小必须与检查点一致
	st, err := os.Stat(outputPath)
	if err != nil {
		return nil, fault.New(fault.KindResume, fault.Rescan, outputPath, 0, -1,
			"output missing for checkpoint: %v", err)
	}
	if st.Size() != c.OutputBytes {
		return nil, fault.New(fault.KindResume, fault.Rescan, outputPath, 0, -1,
			"output tail incomplete: size=%d checkpoint=%d", st.Size(), c.OutputBytes)
	}
	return &c, nil
}
