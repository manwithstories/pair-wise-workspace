// Package segment 基于共享时钟选择关键帧分片边界并输出可复核清单。
// 所有边界时间取自统一媒体时间线；边界相对目标时长的偏差记录为容差，
// 尾分片未达目标时长显式标记，不与正常分片混淆。
package segment

import (
	"crypto/sha256"

	"archivecheck/internal/model"
)

// BoundaryConfig 控制分片切分。
type BoundaryConfig struct {
	TargetNS    int64 // 目标分片时长（纳秒）
	ToleranceNS int64 // 允许的边界时间偏差（纳秒）
	MinKeyGap   int   // 两个分片边界之间最少关键帧数
}

// MediaPoint 是供边界选择使用的帧时间视图。
type MediaPoint struct {
	Seq     uint32
	IsKey   bool
	MediaNS int64
}

// SelectBoundaries 在关键帧约束下选取最接近目标时长的边界序号。
// 每个分片起点必须是关键帧；在容差窗口内选距目标时间最近的关键帧，
// 窗口内无关键帧时取最近的关键帧并保留实际偏差供清单记录。
func SelectBoundaries(points []MediaPoint, cfg BoundaryConfig) []int {
	if len(points) == 0 || cfg.TargetNS <= 0 {
		return nil
	}
	bounds := []int{0}
	nextTarget := points[0].MediaNS + cfg.TargetNS
	lastBound := 0
	for i := 1; i < len(points); i++ {
		if !points[i].IsKey || i-lastBound < cfg.MinKeyGap {
			continue
		}
		// 已越过目标时间：在当前关键帧与前一关键帧之间选更近者。
		if points[i].MediaNS >= nextTarget {
			chosen := i
			if prev := nearestPrevKey(points, i, lastBound); prev >= 0 &&
				abs64(points[prev].MediaNS-nextTarget) < abs64(points[i].MediaNS-nextTarget) {
				chosen = prev
			}
			if chosen > lastBound {
				bounds = append(bounds, chosen)
				lastBound = chosen
				nextTarget = points[chosen].MediaNS + cfg.TargetNS
			}
		}
	}
	// 收尾：若结尾关键帧尚未成为边界，则补为边界，让任何残尾独立成片，
	// 从而能被尾分片不完整规则显式识别而非并入前一满片。
	tail := len(points) - 1
	for tail > lastBound && !points[tail].IsKey {
		tail--
	}
	if tail > lastBound {
		bounds = append(bounds, tail)
	}
	return bounds
}

func nearestPrevKey(points []MediaPoint, i, floor int) int {
	for j := i - 1; j > floor; j-- {
		if points[j].IsKey {
			return j
		}
	}
	return -1
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Build 依据边界把帧切成清单分片。frames 与 points 等长且序号对齐。
// 最后一个分片若短于目标时长，标记 TailIncomplete 并返回 ErrTruncatedTail
// 风格的布尔结果（不中断流程，由调用方决定拒绝或带标记发布）。
func Build(rep string, frames []model.Frame, points []MediaPoint, bounds []int, cfg BoundaryConfig) model.Manifest {
	m := model.Manifest{Rep: rep, MediaSpan: points[len(points)-1].MediaNS - points[0].MediaNS}
	// 满片帧数取首段实际帧数，作为尾分片完整性的结构判据。
	fullFrames := 0
	if len(bounds) > 1 {
		fullFrames = bounds[1] - bounds[0]
	}
	for bi, start := range bounds {
		end := len(points) - 1
		if bi+1 < len(bounds) {
			end = bounds[bi+1] - 1
		}
		frameCount := end - start + 1
		seg := model.Segment{
			Index:    bi,
			Rep:      rep,
			FirstSeq: points[start].Seq,
			LastSeq:  points[end].Seq,
			StartNS:  points[start].MediaNS,
			EndNS:    points[end].MediaNS,
		}
		h := sha256.New()
		for k := start; k <= end; k++ {
			h.Write(frames[k].Payload)
		}
		seg.PayloadSHA256 = h.Sum(nil)
		if bi > 0 {
			seg.ToleranceNS = abs64(points[start].MediaNS -
				(points[bounds[bi-1]].MediaNS + cfg.TargetNS))
		}
		if bi == len(bounds)-1 {
			// 尾分片：帧数少于满片即结构性不完整；或媒体时长超容差偏短。
			frameShort := fullFrames > 0 && frameCount < fullFrames
			timeShort := cfg.TargetNS-(seg.EndNS-seg.StartNS) > cfg.ToleranceNS
			seg.TailIncomplete = frameShort || timeShort
		}
		m.Segments = append(m.Segments, seg)
	}
	return m
}
