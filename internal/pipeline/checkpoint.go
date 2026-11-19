// Package pipeline 编排扫描、变换（重排/修复/索引）与验证三个阶段，
// 支持检查点续跑：缓存超限或中断后，凭输入摘要与水位偏移从损坏位置继续。
package pipeline

import (
	"encoding/json"
	"hash/crc32"
	"os"
	"path/filepath"

	"pairflow/internal/clock"
	"pairflow/internal/rtp"
)

// 输入总量与单轨缓存的规格上限。
const (
	MaxInputTotalBytes int64 = 20 * 1024 * 1024 * 1024
	MaxTrackCacheBytes int64 = 256 * 1024 * 1024
	checkpointName           = "checkpoint.json"
)

// Phase 标识流水线阶段，检查点据此决定续跑入口。
const (
	PhaseRepair = "repair"
	PhaseIndex  = "index"
	PhaseDone   = "done"
)

// DeviceCheckpoint 是单设备的续跑水位。
type DeviceCheckpoint struct {
	DeviceID        string     `json:"device_id"`
	RecordsConsumed int64      `json:"records_consumed"`
	Stats           rtp.Stats  `json:"stats"`
	WorkPCMBytes    int64      `json:"work_pcm_bytes"`
	SidecarEntries  int        `json:"sidecar_entries"`
	LastSeq         uint64     `json:"last_seq"`
	HaveSeq         bool       `json:"have_seq"`
	NextSample      int64      `json:"next_sample"`
	TotalSamples    int64      `json:"total_samples"`
	WrapEvents      int        `json:"wrap_events"`
	Repaired        int        `json:"repaired"`
}

// FileWatermark 记录单个采集文件已消费到的字节偏移。
type FileWatermark struct {
	Name   string `json:"name"`
	Offset int64  `json:"offset"`
	Digest uint32 `json:"digest"`
}

// Checkpoint 是落盘的续跑状态，全部通过临时文件+原子替换写入。
type Checkpoint struct {
	Version      int                       `json:"version"`
	Phase        string                    `json:"phase"`
	InputDigest  string                    `json:"input_digest"`
	InputBytes   int64                     `json:"input_bytes"`
	Watermarks   []FileWatermark           `json:"watermarks"`
	Devices      []DeviceCheckpoint        `json:"devices"`
	UpdatedNano  int64                     `json:"updated_nano"`
}

// SafeJoin 把相对路径限定在 root 内，拒绝任何越界写法。
func SafeJoin(root, rel string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target := filepath.Join(absRoot, rel)
	rel2, err := filepath.Rel(absRoot, target)
	if err != nil || rel2 == ".." || filepath.IsAbs(rel2) ||
		(len(rel2) >= 3 && rel2[:3] == ".."+string(os.PathSeparator)) {
		return "", clock.New("pipeline", clock.KindPathEscape, "", 0, clock.NoSeq,
			"path escapes output directory: "+rel, "仅允许写入指定输出目录")
	}
	return target, nil
}

// writeJSONAtomic 先写临时文件再原子替换。
func writeJSONAtomic(path string, v any) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cp-*.tmp")
	if err != nil {
		return clock.Wrap("pipeline.checkpoint", clock.KindCacheLimit, "", 0,
			clock.NoSeq, "create checkpoint temp failed", "检查输出目录权限", err)
	}
	name := tmp.Name()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// Save 原子写出检查点。
func (c *Checkpoint) Save(outDir string) error {
	p, err := SafeJoin(outDir, checkpointName)
	if err != nil {
		return err
	}
	return writeJSONAtomic(p, c)
}

// LoadCheckpoint 读取检查点；不存在返回 (nil, nil)。
func LoadCheckpoint(outDir string) (*Checkpoint, error) {
	p, err := SafeJoin(outDir, checkpointName)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, clock.Wrap("pipeline.checkpoint", clock.KindCheckpointMismatch, "",
			0, clock.NoSeq, "checkpoint unreadable", "删除检查点后全量重跑", err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, clock.Wrap("pipeline.checkpoint", clock.KindCheckpointMismatch, "",
			0, clock.NoSeq, "checkpoint json invalid", "删除检查点后全量重跑", err)
	}
	return &cp, nil
}

// VerifyDigest 重新计算输入摘要并与检查点对账；不一致返回结构化错误。
func (c *Checkpoint) VerifyDigest(digest string, inputBytes int64) error {
	if digest != c.InputDigest || inputBytes != c.InputBytes {
		return clock.New("pipeline.checkpoint", clock.KindCheckpointMismatch, "",
			0, clock.NoSeq,
			"input digest or size changed since checkpoint",
			"输入已变化，必须删除旧检查点后全量重跑")
	}
	return nil
}

// digestFiles 按文件名顺序对采集文件做增量 CRC32，返回十六进制摘要与总字节。
func digestFiles(paths []string) (string, int64, error) {
	h := crc32.NewIEEE()
	var total int64
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return "", 0, clock.Wrap("pipeline.scan", clock.KindTruncatedHeader, "",
				0, clock.NoSeq, "capture file unreadable: "+p, "检查输入路径", err)
		}
		if _, err := h.Write(raw); err != nil {
			return "", 0, err
		}
		total += int64(len(raw))
	}
	if total > MaxInputTotalBytes {
		return "", total, clock.New("pipeline.scan", clock.KindInputTooLarge, "", 0,
			clock.NoSeq, "input exceeds 20GB limit", "拆分输入后重试")
	}
	return formatDigest(h.Sum32()), total, nil
}

func formatDigest(v uint32) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 8)
	for i := 0; i < 8; i++ {
		out[7-i] = hex[v&0xf]
		v >>= 4
	}
	return string(out)
}
