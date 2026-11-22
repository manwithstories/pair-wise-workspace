// Package pipeline 把解析恢复后的多轨素材按共同时间线切分为可剪辑片段，
// 计算静音区间、逐片段指纹并输出来源与恢复摘要。所有结果均可重跑核验。
package pipeline

import (
	"math"
)

// SilenceKind 标记静音区间的可信程度。
const (
	SilenceKnown   = "silent"   // 基于 PCM/统计确认
	SilenceUnknown = "unknown"  // 缺样本，无法判定
	SilenceGap     = "gap"      // 轨道样本之间的空缺
)

// SilenceInterval 是共同时间线上的一个静音/未知区间。
type SilenceInterval struct {
	StartNs int64  `json:"start_ns"`
	EndNs   int64  `json:"end_ns"`
	Kind    string `json:"kind"`
	Ms      int64  `json:"ms"`
}

// SilenceConfig 控制静音判定。
type SilenceConfig struct {
	// ThresholdDb 低于该峰值（dBFS）视为静音，默认 -50。
	ThresholdDb float64
	// MinMergeMs 短于该时长的静音并入相邻区间，默认 200ms。
	MinMergeMs int64
	// MaxGapMs 样本间超过该缺口才标记 unknown，默认 500ms。
	MaxGapMs int64
}

// DefaultSilenceConfig 返回现场剪辑的保守默认值。
func DefaultSilenceConfig() SilenceConfig {
	return SilenceConfig{ThresholdDb: -50, MinMergeMs: 200, MaxGapMs: 500}
}

// PCMFrame 是一段可直接计算电平的 PCM 音频。
type PCMFrame struct {
	StartNs    int64
	DurationNs int64 // 该帧在共同时间线上的时长
	Samples    []int16
	Channels   int
}

// LevelDbFS 返回帧的峰值 dBFS；空帧返回 -Inf。
func LevelDbFS(f PCMFrame) float64 {
	peak := 0
	for _, s := range f.Samples {
		v := int(s)
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	if peak == 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(float64(peak)/32768.0)
}

// DetectSilencePCM 基于 PCM 帧计算静音区间。
// 帧之间的时间缺口超过 MaxGapMs 时标为 unknown；短静音按 MinMergeMs 合并。
func DetectSilencePCM(frames []PCMFrame, cfg SilenceConfig) []SilenceInterval {
	var raw []SilenceInterval
	for i, f := range frames {
		end := f.StartNs + frameDurationNs(f)
		if i > 0 {
			prevEnd := frames[i-1].StartNs + frameDurationNs(frames[i-1])
			if gap := f.StartNs - prevEnd; gap > cfg.MaxGapMs*1_000_000 {
				raw = append(raw, SilenceInterval{
					StartNs: prevEnd, EndNs: f.StartNs, Kind: SilenceUnknown,
					Ms: nsToMs(gap),
				})
			}
		}
		if LevelDbFS(f) <= cfg.ThresholdDb {
			raw = append(raw, SilenceInterval{
				StartNs: f.StartNs, EndNs: end, Kind: SilenceKnown,
				Ms: nsToMs(end - f.StartNs),
			})
		}
	}
	return mergeIntervals(raw, cfg.MinMergeMs)
}

// CompressedStat 是压缩音频轨可获得的粗粒度统计（无 PCM 时使用）。
type CompressedStat struct {
	StartNs    int64
	EndNs      int64
	Silent     bool // 由编码器/容器统计给出的静音标志
	Available  bool // 统计是否可信
}

// DetectSilenceStats 使用压缩音频统计计算静音；不可用区间标 unknown。
func DetectSilenceStats(stats []CompressedStat, cfg SilenceConfig) []SilenceInterval {
	var raw []SilenceInterval
	for _, s := range stats {
		if !s.Available {
			raw = append(raw, SilenceInterval{StartNs: s.StartNs, EndNs: s.EndNs,
				Kind: SilenceUnknown, Ms: nsToMs(s.EndNs - s.StartNs)})
			continue
		}
		if s.Silent {
			raw = append(raw, SilenceInterval{StartNs: s.StartNs, EndNs: s.EndNs,
				Kind: SilenceKnown, Ms: nsToMs(s.EndNs - s.StartNs)})
		}
	}
	return mergeIntervals(raw, cfg.MinMergeMs)
}

// mergeIntervals 合并相邻同类型区间，并把短于阈值的静音并入相邻段。
func mergeIntervals(in []SilenceInterval, minMergeMs int64) []SilenceInterval {
	if len(in) == 0 {
		return nil
	}
	merged := []SilenceInterval{in[0]}
	for _, cur := range in[1:] {
		last := &merged[len(merged)-1]
		if cur.Kind == last.Kind && cur.StartNs <= last.EndNs+1_000_000 {
			last.EndNs = max64(last.EndNs, cur.EndNs)
			last.Ms = nsToMs(last.EndNs - last.StartNs)
			continue
		}
		merged = append(merged, cur)
	}
	// 过短的确认静音不单独成段，并入前一段（避免噪声式碎片化报告）。
	out := make([]SilenceInterval, 0, len(merged))
	for _, iv := range merged {
		if iv.Kind == SilenceKnown && iv.Ms < minMergeMs && len(out) > 0 {
			prev := &out[len(out)-1]
			prev.EndNs = iv.EndNs
			prev.Ms = nsToMs(prev.EndNs - prev.StartNs)
			continue
		}
		out = append(out, iv)
	}
	return out
}

// TotalSilenceMs 汇总确认静音的毫秒数（unknown 不计入）。
func TotalSilenceMs(ivs []SilenceInterval) int64 {
	var total int64
	for _, iv := range ivs {
		if iv.Kind == SilenceKnown {
			total += iv.Ms
		}
	}
	return total
}

func frameDurationNs(f PCMFrame) int64 {
	if f.DurationNs > 0 {
		return f.DurationNs
	}
	ch := f.Channels
	if ch <= 0 {
		ch = 1
	}
	// 无显式时长时按 48kHz 单帧估算（仅兜底，生成器始终显式提供）。
	return int64(len(f.Samples)/ch) * 1_000_000_000 / 48_000
}
func nsToMs(ns int64) int64 { return ns / 1_000_000 }

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
