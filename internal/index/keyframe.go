// Package index 负责关键帧索引的重建与校验。
//
// 当 stss 缺页（索引缺失）时，索引器只在“可解析且完整”的样本区间内
// 重建关键帧标记；截断区间被显式标记为不可信，绝不伪造尾部关键帧。
package index

import (
	"fmt"

	"mediad/internal/fault"
	"mediad/internal/model"
)

// KeyframeIndex 是单条轨道的关键帧索引重建结果。
type KeyframeIndex struct {
	TrackID string
	// Keyframes 为关键帧样本号（1 基），按升序排列。
	Keyframes []uint32
	// Rebuilt 为 true 表示原始 stss 缺页，索引由样本扫描重建。
	Rebuilt bool
	// IncompleteRanges 是无法确认关键帧状态的 PTS 区间（闭开）。
	IncompleteRanges [][2]int64
	// Complete 为 true 表示全轨道样本均可解析，索引覆盖完整时间轴。
	Complete bool
}

// HasKeyframe 判断给定样本号是否在索引中（二分查找）。
func (k *KeyframeIndex) HasKeyframe(sampleNo uint32) bool {
	lo, hi := 0, len(k.Keyframes)
	for lo < hi {
		mid := (lo + hi) / 2
		if k.Keyframes[mid] < sampleNo {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo < len(k.Keyframes) && k.Keyframes[lo] == sampleNo
}

// Rebuild 根据样本流重建或校验关键帧索引。
//
// stssPresent=false 表示原始索引缺页：此时仅当样本自身带可信关键帧
// 标记（如 RTP marker/合成夹具的周期性标记）时才收录；对不完整样本
// 只记录受影响区间，不产生任何关键帧条目。
func Rebuild(trackID string, samples []model.Sample, stssPresent bool) *KeyframeIndex {
	idx := &KeyframeIndex{TrackID: trackID, Rebuilt: !stssPresent, Complete: true}
	var gapStart int64
	inGap := false
	for i := range samples {
		s := &samples[i]
		if !s.Complete {
			idx.Complete = false
			if !inGap {
				gapStart = s.PTS
				inGap = true
			}
			continue
		}
		if inGap {
			idx.IncompleteRanges = append(idx.IncompleteRanges,
				[2]int64{gapStart, s.PTS})
			inGap = false
		}
		if s.Keyframe {
			idx.Keyframes = append(idx.Keyframes, s.Index)
		}
	}
	if inGap {
		// 尾部截断：区间延伸到最后一个样本的声明结束位置，
		// 关键帧列表在截断点之前终止，不伪造尾部关键帧。
		last := samples[len(samples)-1]
		idx.IncompleteRanges = append(idx.IncompleteRanges,
			[2]int64{gapStart, last.PTS + int64(last.Duration)})
	}
	return idx
}

// Verify 校验既有 stss 条目与样本扫描结果一致，返回不一致的可恢复故障。
func Verify(idx *KeyframeIndex, declared []uint32) []*fault.Error {
	want := map[uint32]bool{}
	for _, k := range idx.Keyframes {
		want[k] = true
	}
	got := map[uint32]bool{}
	var errs []*fault.Error
	for _, k := range declared {
		got[k] = true
		if !want[k] {
			errs = append(errs, fault.Recoverable(fault.CodeIndexMissing,
				"index.Verify", 0, 0,
				fmt.Errorf("stss declares keyframe %d absent from samples", k)))
		}
	}
	for k := range want {
		if !got[k] {
			errs = append(errs, fault.Recoverable(fault.CodeIndexMissing,
				"index.Verify", 0, 0,
				fmt.Errorf("keyframe %d missing from stss", k)))
		}
	}
	return errs
}

// HitRate 统计样本流中关键帧标记被索引命中的比例，返回命中数与总数。
func HitRate(idx *KeyframeIndex, samples []model.Sample) (hits, total int) {
	for i := range samples {
		s := &samples[i]
		if !s.Keyframe {
			continue
		}
		total++
		if idx.HasKeyframe(s.Index) {
			hits++
		}
	}
	return hits, total
}
