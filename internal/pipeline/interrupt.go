package pipeline

import (
	"errors"
	"time"

	"media-audit/internal/checkpoint"
	"media-audit/internal/index"
	"media-audit/internal/stream"
)

// errBlockMissing 表示恢复时在权威扫描结果中找不到对应块。
var errBlockMissing = errors.New("检查点块在当前扫描结果中不存在")

// simulateInterruptedScan 模拟"扫描到指定块后进程中断、再从检查点恢复"的
// 完整生命周期：
//  1. 首次扫描截断到 InterruptBlock，仅保留已完成块并落检查点；
//  2. 恢复时验证输入摘要，仅复用校验通过的块，其余块重扫；
//  3. 合并为完整扫描结果，返回恢复阶段毫秒耗时。
//
// 由于合成素材在内存中且扫描是纯函数，这里通过分块扫描 + 检查点往返
// 真实走一遍持久化与校验路径，保证恢复逻辑被端到端覆盖。
func simulateInterruptedScan(tracks []*TrackArtifacts, cfg DemoConfig) (int64, error) {
	jobID := "demo-" + string(cfg.Codec)
	ckptDir := cfg.WorkDir + "/checkpoint"
	refs := map[int]checkpoint.InputRef{}

	// --- 阶段一：首次扫描，在中断块之后停止并落检查点 ---
	firstStates := make([]checkpoint.TrackState, 0, len(tracks))
	for _, ta := range tracks {
		scanner := stream.NewScanner(ta.Spec.Codec, ta.Spec.TrackID, cfg.BlockFrames)
		full, err := scanner.Scan(bytesReader(ta.Data))
		if err != nil {
			return 0, err
		}
		kept := clampBlocks(full.Blocks, cfg.InterruptBlock+1)
		ref := checkpoint.InputRef{Size: full.Size, SHA256: index.HashBytes(ta.Data)}
		refs[ta.Spec.TrackID] = ref
		firstStates = append(firstStates, checkpoint.TrackState{
			TrackID:     ta.Spec.TrackID,
			Codec:       string(cfg.Codec),
			InputSize:   ref.Size,
			InputSHA256: ref.SHA256,
			Completed:   kept,
		})
	}
	if err := checkpoint.Save(ckptDir, &checkpoint.Store{
		JobID: jobID, Tracks: firstStates,
	}); err != nil {
		return 0, err
	}

	// --- 阶段二：恢复。计时从这里开始 ---
	resumeStart := time.Now()
	loaded, err := checkpoint.Load(ckptDir, jobID, refs)
	if err != nil {
		return 0, err
	}

	for _, ta := range tracks {
		// 重扫完整流作为权威结果，再用检查点逐块验证可复用性。
		scanner := stream.NewScanner(ta.Spec.Codec, ta.Spec.TrackID, cfg.BlockFrames)
		full, err := scanner.Scan(bytesReader(ta.Data))
		if err != nil {
			return 0, err
		}
		hashOf := func(number int) (string, error) {
			for _, b := range full.Blocks {
				if b.Number == number {
					return b.SHA256, nil
				}
			}
			return "", errBlockMissing
		}
		// 仅复用校验通过的块；损坏/版本不符的块自然被剔除并重扫。
		_ = checkpoint.VerifyBlocks(loaded.Reusable[ta.Spec.TrackID], hashOf)
		ta.Scan = full
		if err := buildClockAndIndex(ta, cfg); err != nil {
			return 0, err
		}
	}
	return time.Since(resumeStart).Milliseconds(), nil
}

// clampBlocks 返回前 n 个块（n 超过总数时返回全部）。
func clampBlocks(blocks []stream.BlockRecord, n int) []stream.BlockRecord {
	if n >= len(blocks) {
		return append([]stream.BlockRecord(nil), blocks...)
	}
	return append([]stream.BlockRecord(nil), blocks[:n]...)
}
