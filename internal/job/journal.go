package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"

	"rcx/internal/fixture"
)

// SegCheckpoint 记录一个已完成段的检查点。
type SegCheckpoint struct {
	Index       int       `json:"index"`
	Status      string    `json:"status"` // done | unreachable
	QP          float64   `json:"qp"`
	Bits        int64     `json:"bits"`
	Checksum    uint64    `json:"checksum"`
	PSNRMeanDB  float64   `json:"psnr_mean_db"`
	PSNRStdDB   float64   `json:"psnr_std_db"`
	VBVMinRatio float64   `json:"vbv_min_ratio"`
	FrameBits   []int64   `json:"frame_bits"`
	FramePSNR   []float64 `json:"frame_psnr"`
}

// Journal 是任务级检查点文件。
type Journal struct {
	Version  int             `json:"version"`
	State    State           `json:"state"`
	Segments []SegCheckpoint `json:"segments"`
}

// ChecksumFrames 计算一段源帧的 FNV-1a 校验和。
func ChecksumFrames(frames []*fixture.Frame) uint64 {
	h := fnv.New64a()
	for _, f := range frames {
		h.Write(f.Y)
		h.Write(f.U)
		h.Write(f.V)
	}
	return h.Sum64()
}

// Save 原子写入检查点：先写临时文件再 rename，旧版本保留为 .bak。
func Save(path string, j *Journal) error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", old, 0o644); err != nil {
			return fmt.Errorf("备份旧检查点失败: %w", err)
		}
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load 读取检查点；主文件缺失或半写损坏时回退到 .bak。
// 第二个返回值表示是否存在可用检查点。
func Load(path string) (*Journal, bool, error) {
	for _, p := range []string{path, path + ".bak"} {
		data, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		var j Journal
		if err := json.Unmarshal(data, &j); err != nil {
			continue // 半写损坏，回退上一完整版本
		}
		return &j, true, nil
	}
	return &Journal{Version: 1, State: StatePending}, false, nil
}
