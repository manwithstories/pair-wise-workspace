package pipeline

import (
	"crypto/sha256"
	"encoding/binary"

	"archivecheck/internal/fixture"
	"archivecheck/internal/model"
)

// ParamsHash 对影响恢复结果的运行参数做独立摘要，与输入样本摘要分开绑定。
func ParamsHash(c Config) [32]byte {
	h := sha256.New()
	var b [8]byte
	putU64 := func(v uint64) {
		binary.BigEndian.PutUint64(b[:], v)
		h.Write(b[:])
	}
	putU4 := func(v uint32) {
		var q [4]byte
		binary.BigEndian.PutUint32(q[:], v)
		h.Write(q[:])
	}
	putU64(uint64(c.SegmentTargetNS))
	putU64(uint64(c.BoundaryTolNS))
	putU64(uint64(c.WindowSize))
	putU64(uint64(c.Fault.LossEvery))
	putU64(uint64(c.Fault.ReorderEvery))
	putU64(uint64(c.Fault.DupeEvery))
	putU4(boolToU32(c.CrossModuleFault))
	var d [32]byte
	copy(d[:], h.Sum(nil))
	return d
}

func boolToU32(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// applyCrossModuleFault 同时制造两类故障：
//  1. 720p 层中段音频锚点出现 500ms 时钟突变（隔离时钟段）；
//  2. 360p 层在同一区域删除一个关键帧，使该层边界选择与 720p 冲突。
//
// 两者跨模块叠加，用于验证汇合核验必须拒绝发布。
func applyCrossModuleFault(frames map[string][]model.Frame, p fixture.Params) {
	if hi, ok := frames["720p"]; ok && len(hi) > 200 {
		jump := len(hi) / 2
		for i := jump; i < len(hi); i++ {
			hi[i].AudioSamples += uint348kHalfSecond(p)
		}
	}
	if lo, ok := frames["360p"]; ok && len(lo) > 200 {
		kill := len(lo) / 2
		// 把关键帧降级为普通帧且不改哈希（元数据层面的边界冲突）。
		lo[kill].IsKey = false
	}
}

// uint348kHalfSecond 返回 48kHz 下 500ms 的采样数。
func uint348kHalfSecond(_ fixture.Params) uint32 { return 24000 }

// applyTailTruncate 确定性地把每个表示层截到一个关键帧分片的中段结束，
// 使尾分片只含约半组帧（约 1s），显著短于 2s 目标且超出 100ms 容差，
// 从而稳定触发尾分片不完整标记与发布拒绝。
func applyTailTruncate(frames map[string][]model.Frame) {
	for rep, fs := range frames {
		// 对齐到 60 帧分片边界后只保留 30 帧残尾，保证切点不在分片起点。
		cut := (len(fs)/60)*60 + 30
		if cut >= len(fs) {
			cut = len(fs) - 30
		}
		if cut < 2 {
			continue
		}
		frames[rep] = fs[:cut]
	}
}
