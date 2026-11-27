// Package pipeline 编排扫描、接入、合轨、渲染、清单全链路，并提供
// 可校验摘要的断点续跑能力。
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"medpipe/internal/clock"
)

// Checkpoint 记录一次运行的确定性进度，供 resume 校验。
type Checkpoint struct {
	Schema       string            `json:"schema"`
	ParamsHash   string            `json:"params_hash"`
	LastBlock    int64             `json:"last_block"`    // 已完整处理的块号
	LastPacket   int64             `json:"last_packet"`   // 已提交包序号水位
	Chain        string            `json:"chain_sha256"`  // 已处理包的链式摘要
	BlockDigests map[string]string `json:"block_digests"` // 块号 -> 块文件摘要
	FramesDone   int64             `json:"frames_done"`
	Interrupted  bool              `json:"interrupted"`
	// Packets 保存已提交包的完整内容，resume 时原样重放，
	// 保证静音补偿与故障丢弃和完整运行逐字节一致。
	Packets []SavedPacket `json:"packets"`
}

// SavedPacket 是检查点中可序列化的已提交包。
type SavedPacket struct {
	Seq        int64  `json:"seq"`
	BlockID    int64  `json:"block_id"`
	StartFrame int64  `json:"start_frame"`
	Frames     int64  `json:"frames"`
	Source     string `json:"source"`
	SHA256     string `json:"sha256"`
	PCM        []byte `json:"pcm"`
}

const checkpointSchema = "medpipe/checkpoint/v1"

// hashParams 对运行参数做稳定摘要，参数变化则检查点失效。
func hashParams(params map[string]string) string {
	h := sha256.New()
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	// 简单排序保证键序稳定
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte("="))
		h.Write([]byte(params[k]))
		h.Write([]byte(";"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// newCheckpoint 以运行参数初始化检查点。
func newCheckpoint(params map[string]string) *Checkpoint {
	return &Checkpoint{
		Schema:       checkpointSchema,
		ParamsHash:   hashParams(params),
		LastBlock:    -1,
		LastPacket:   -1,
		Chain:        hex.EncodeToString(sha256.New().Sum(nil)),
		BlockDigests: map[string]string{},
	}
}

// fold 把一个已提交包的摘要并入链式哈希，并保存其完整内容。
func (c *Checkpoint) fold(packetSHA string, p SavedPacket) {
	h := sha256.New()
	h.Write([]byte(c.Chain))
	h.Write([]byte(packetSHA))
	c.Chain = hex.EncodeToString(h.Sum(nil))
	c.LastPacket++
	c.Packets = append(c.Packets, p)
}

// save 原子写入检查点（先临时文件再 rename）。
func (c *Checkpoint) save(dir string) error {
	path, err := safeCheckpointPath(dir, "checkpoint.json")
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return clock.WrapError(clock.CodeCheckpoint, -1, err, "检查点序列化失败")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return clock.WrapError(clock.CodeCheckpoint, -1, err, "检查点临时文件写入失败")
	}
	if err := os.Rename(tmp, path); err != nil {
		return clock.WrapError(clock.CodeCheckpoint, -1, err, "检查点落盘失败")
	}
	return nil
}

// loadCheckpoint 读取并校验检查点 schema 与参数摘要。
func loadCheckpoint(dir string, params map[string]string) (*Checkpoint, error) {
	path, err := safeCheckpointPath(dir, "checkpoint.json")
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, clock.WrapError(clock.CodeCheckpoint, -1, err, "检查点读取失败")
	}
	var c Checkpoint
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, clock.WrapError(clock.CodeCheckpoint, -1, err, "检查点解析失败")
	}
	if c.Schema != checkpointSchema {
		return nil, clock.NewError(clock.CodeCheckpoint, -1, "检查点 schema 不匹配: %s", c.Schema)
	}
	if c.ParamsHash != hashParams(params) {
		return nil, clock.NewError(clock.CodeCheckpoint, -1, "运行参数变化，检查点失效需完整重跑")
	}
	return &c, nil
}

func safeCheckpointPath(dir, name string) (string, error) {
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) || clean == ".." {
		return "", clock.NewError(clock.CodePathRejected, -1, "检查点非法路径 %s", name)
	}
	return filepath.Join(dir, clean), nil
}
