package journal

import (
	"encoding/json"
	"os"
	"path/filepath"

	"archivemedia/internal/media"
)

// Manifest 是一次归档任务的校验清单，覆盖输入摘要、处理统计与输出摘要。
// 固定输入重复运行（含断点续跑）必须产出逐字节一致的清单。
type Manifest struct {
	// Version 是清单格式版本。
	Version int `json:"version"`
	// InputDigest 是输入事件集合的确定性 SHA-256（十六进制）。
	InputDigest string `json:"input_digest"`
	// InputPackets 是计入结果的唯一包数。
	InputPackets uint64 `json:"input_packets"`
	// Duplicates 是同键同内容重复次数。
	Duplicates uint64 `json:"duplicates"`
	// Conflicts 是同键异内容冲突次数。
	Conflicts uint64 `json:"conflicts"`
	// Missing 是仍为占位（未被重传修复）的区间数。
	Missing uint64 `json:"missing"`
	// Corrupt 是损坏/不可解码包数。
	Corrupt uint64 `json:"corrupt"`
	// OutputFile 是输出 WAV 文件名（不含目录，保证可移植）。
	OutputFile string `json:"output_file"`
	// OutputOffset 是输出总字节数。
	OutputOffset int64 `json:"output_offset"`
	// OutputDigest 是输出文件的 SHA-256（十六进制）。
	OutputDigest string `json:"output_digest"`
	// IntegratedLUFS 是归一后集成响度。
	IntegratedLUFS float64 `json:"integrated_lufs"`
	// TruePeakDBTP 是归一后真峰值。
	TruePeakDBTP float64 `json:"true_peak_dbtp"`
	// Resume 标记本次是否为断点续跑。
	Resume bool `json:"resume"`
}

// NewManifest 构造带当前版本号的空清单。
func NewManifest() *Manifest { return &Manifest{Version: 1} }

// Write 原子写入清单 JSON（缩进固定，保证可 diff 与可复现）。
func (m *Manifest) Write(path string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return media.E(media.KindCheckpoint, "journal.manifest-write",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"清单序列化失败", err)
	}
	data = append(data, '\n')
	if err := atomicWrite(path, data); err != nil {
		return media.Wrap(err, "journal.manifest-write", "清单落盘失败")
	}
	return nil
}

// LoadManifest 读取清单。
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, media.E(media.KindCheckpoint, "journal.manifest-load",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"读取清单失败", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, media.E(media.KindCheckpoint, "journal.manifest-load",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"清单解析失败", err)
	}
	return &m, nil
}

// Verify 比对两份清单的输入与输出摘要，任何不一致都返回 KindCheckpoint 错误，
// 由终端映射为退出码 4。
func (m *Manifest) Verify(other *Manifest) error {
	if other == nil {
		return media.E(media.KindCheckpoint, "journal.manifest-verify",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"对照清单为空", nil)
	}
	if m.InputDigest != other.InputDigest {
		return media.E(media.KindCheckpoint, "journal.manifest-verify",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"输入摘要不一致", nil)
	}
	if m.InputPackets != other.InputPackets {
		return media.E(media.KindCheckpoint, "journal.manifest-verify",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"计入包数不一致", nil)
	}
	if m.OutputDigest != other.OutputDigest {
		return media.E(media.KindCheckpoint, "journal.manifest-verify",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"输出摘要不一致（断点续跑与全新跑结果必须相同）", nil)
	}
	if m.OutputOffset != other.OutputOffset {
		return media.E(media.KindCheckpoint, "journal.manifest-verify",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"输出偏移不一致", nil)
	}
	return nil
}

// ManifestPath 返回目录下的标准清单路径。
func ManifestPath(dir string) string {
	return filepath.Join(dir, "manifest.json")
}
