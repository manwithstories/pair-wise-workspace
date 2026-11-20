// Package align 把多个码率表示层的关键帧对齐为共同可解码切点。
// 切点必须落在每层的随机访问帧上；层间最大偏差超过阈值时显式标记
// 失败，绝不静默挪动切点。
package align

import (
	"errors"
	"sort"
)

// DefaultMaxDriftMicros 是默认允许的层间切点最大偏差（40 毫秒）。
const DefaultMaxDriftMicros int64 = 40_000

// Frame 是一个表示层上的可对齐帧（时间单位为统一时间轴微秒）。
type Frame struct {
	AUIndex   int
	Micros    int64
	Keyframe  bool
	ByteStart int64
	ByteEnd   int64
}

// Representation 是一条表示层的对齐输入。
type Representation struct {
	ID     string
	Frames []Frame
}

// Cut 是一个跨所有表示层的共同切点。
type Cut struct {
	Index       int                    `json:"index"`
	AnchorMicros int64                 `json:"anchor_micros"`
	MaxDriftMicros int64               `json:"max_drift_micros"`
	Points      map[string]FramePoint  `json:"points"`
}

// FramePoint 是某表示层在该切点上实际选用的帧。
type FramePoint struct {
	AUIndex      int   `json:"au_index"`
	Micros       int64 `json:"micros"`
	DriftMicros  int64 `json:"drift_micros"`
	ByteStart    int64 `json:"byte_start"`
	ByteEnd      int64 `json:"byte_end"`
}

// Failure 描述一次无法对齐的切点及其原因。
type Failure struct {
	NearMicros int64  `json:"near_micros"`
	Reason     string `json:"reason"`
	DriftMicros int64 `json:"drift_micros"`
}

// Result 是对齐结果。
type Result struct {
	Cuts         []Cut
	Failures     []Failure
	MaxDrift     int64 // 所有成功切点中的最大层间偏差
	ThresholdUS  int64
}

// Options 控制对齐行为。
type Options struct {
	MaxDriftMicros int64
	// TargetSegmentUS 为目标分片时长（微秒），决定切点密度。
	TargetSegmentUS int64
}

// abs64 返回 int64 绝对值。
func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Align 在所有表示层的关键帧上求共同切点。以第一层关键帧时间为锚，
// 在其余每层选择距锚最近且不超过阈值的关键帧；任一层超阈即记为失败。
func Align(reps []Representation, opts Options) (*Result, error) {
	if len(reps) == 0 {
		return nil, errors.New("没有可对齐的表示层")
	}
	if opts.MaxDriftMicros <= 0 {
		opts.MaxDriftMicros = DefaultMaxDriftMicros
	}
	if opts.TargetSegmentUS <= 0 {
		opts.TargetSegmentUS = 10_000_000
	}

	// 每层提取有序关键帧。
	keyframes := make([][]Frame, len(reps))
	for i, rep := range reps {
		for _, f := range rep.Frames {
			if f.Keyframe {
				keyframes[i] = append(keyframes[i], f)
			}
		}
		if len(keyframes[i]) == 0 {
			return nil, errors.New("表示层 " + rep.ID + " 缺少随机访问帧")
		}
	}

	res := &Result{ThresholdUS: opts.MaxDriftMicros}
	// 以第一层关键帧为锚序列，并按目标分片时长抽样，避免切点过密。
	var anchors []int64
	lastEmitted := int64(-1 << 62)
	for _, kf := range keyframes[0] {
		if kf.Micros-lastEmitted >= opts.TargetSegmentUS || len(anchors) == 0 {
			anchors = append(anchors, kf.Micros)
			lastEmitted = kf.Micros
		}
	}

	cutIdx := 0
	for _, anchor := range anchors {
		cut := Cut{
			Index:        cutIdx,
			AnchorMicros: anchor,
			Points:       map[string]FramePoint{},
		}
		var maxDrift int64
		ok := true
		for ri, rep := range reps {
			fp, drift, found := nearestKeyframe(keyframes[ri], anchor)
			if !found || abs64(drift) > opts.MaxDriftMicros {
				ok = false
				res.Failures = append(res.Failures, Failure{
					NearMicros:  anchor,
					Reason:      "表示层 " + rep.ID + " 无落在偏差阈值内的随机访问帧",
					DriftMicros: abs64(drift),
				})
				break
			}
			cut.Points[rep.ID] = FramePoint{
				AUIndex:     fp.AUIndex,
				Micros:      fp.Micros,
				DriftMicros: drift,
				ByteStart:   fp.ByteStart,
				ByteEnd:     fp.ByteEnd,
			}
			if abs64(drift) > maxDrift {
				maxDrift = abs64(drift)
			}
		}
		if !ok {
			continue
		}
		cut.MaxDriftMicros = maxDrift
		if maxDrift > res.MaxDrift {
			res.MaxDrift = maxDrift
		}
		res.Cuts = append(res.Cuts, cut)
		cutIdx++
	}
	sort.Slice(res.Failures, func(i, j int) bool {
		return res.Failures[i].NearMicros < res.Failures[j].NearMicros
	})
	return res, nil
}

// nearestKeyframe 在有序关键帧中找距 anchor 最近的帧，返回帧与漂移。
func nearestKeyframe(kfs []Frame, anchor int64) (Frame, int64, bool) {
	if len(kfs) == 0 {
		return Frame{}, 0, false
	}
	// 二分查找首个 Micros >= anchor 的关键帧。
	idx := sort.Search(len(kfs), func(i int) bool {
		return kfs[i].Micros >= anchor
	})
	best := kfs[0]
	if idx < len(kfs) {
		best = kfs[idx]
	}
	if idx > 0 {
		prev := kfs[idx-1]
		if abs64(prev.Micros-anchor) <= abs64(best.Micros-anchor) {
			best = prev
		}
	}
	return best, best.Micros - anchor, true
}
