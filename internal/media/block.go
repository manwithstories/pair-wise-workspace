// Package media 定义各子系统之间流转的采样块与缺口模型。
package media

import (
	"fmt"

	"media-pipeline/internal/clock"
	"media-pipeline/internal/fault"
)

// Gap 记录时间线上一段真实缺失的采样帧区间。
// 流水线只保留缺口证据，绝不伪造静音样本填补。
type Gap struct {
	FirstSample uint64 // 缺口起始的绝对采样帧序号
	Frames      uint64 // 缺失帧数
	Reason      string // 例如 "rtp-burst-loss"
	FirstSeq    uint16 // 关联的首个 RTP 序号
	LastSeq     uint16 // 关联的末个 RTP 序号
}

// Block 是经过重排/校验后交给分析阶段的连续 PCM 采样块。
type Block struct {
	ID          string            // 源块标识
	Format      clock.Timebase    // 共享时间基准
	FirstSample uint64            // 本块首帧在全局时间线上的序号
	PCM         []byte            // 真实采样字节（不含任何伪造帧）
	Gaps        []Gap             // 本块时间线内保留的缺口证据
	SourceSha   [32]byte          // 原始输入字节的 SHA-256
	Meta        map[string]string // 附加溯源信息
}

// FrameCount 返回 PCM 载荷实际包含的整帧数；非整帧则报错并给出字节偏移。
func (b Block) FrameCount() (uint64, error) {
	if err := b.Format.Valid(); err != nil {
		return 0, err
	}
	frames, rem, err := b.Format.BytesToFrames(int64(len(b.PCM)))
	if err != nil {
		lastGood := int64(len(b.PCM)) - int64(rem)
		return 0, fault.New(fault.KindMalformedInput, b.ID,
			lastGood, -1, true,
			"frame count: non-frame-aligned tail").Wrap(err)
	}
	return frames, nil
}

// EndSample 返回本块最后一帧之后的采样序号（不含缺口填充帧）。
func (b Block) EndSample() uint64 {
	return b.FirstSample + uint64(len(b.PCM)/b.Format.FrameSize())
}

// String 便于日志与测试诊断。
func (b Block) String() string {
	return fmt.Sprintf("block %s first=%d bytes=%d gaps=%d",
		b.ID, b.FirstSample, len(b.PCM), len(b.Gaps))
}
