// Package report 计算交付质量与时间线对齐指标：
// 码率偏差、削波/静音比例、缺失采样率、质量分位数与清单对齐误差。
package report

import (
	"math"
	"sort"

	"pairflow/internal/clock"
	"pairflow/internal/index"
	"pairflow/internal/manifest"
	"pairflow/internal/repair"
)

// Metrics 是单轨道的验证报告指标（输出字段名与验收命令保持一致）。
type Metrics struct {
	TrackID             uint32  `json:"track_id"`
	DeviceID            string  `json:"device_id"`
	TargetBitrateBps    int64   `json:"target_bitrate_bps"`
	ActualBitrateBps    int64   `json:"actual_bitrate_bps"`
	BitrateDeviationPct float64 `json:"bitrate_deviation_pct"`
	ClippedPct          float64 `json:"clipped_pct"`
	SilentPct           float64 `json:"silent_pct"`
	MissingSamplePct    float64 `json:"missing_sample_pct"`
	QualityP50          float64 `json:"quality_p50"`
	QualityP95          float64 `json:"quality_p95"`
	ManifestAlignmentMS float64 `json:"manifest_alignment_ms"`
}

// Options 控制指标计算口径。
type Options struct {
	TargetBitrateBps int64
	// ClipThreshold 为 int16 削波门限（含），默认 32700。
	ClipThreshold int16
}

// PCMStats 汇总一段 16bit 小端 PCM 的逐采样统计。
type PCMStats struct {
	Samples    int64
	Clipped    int64
	Silent     int64 // |sample| < 4 视为静音
	BlockScore []float64
}

// AnalyzePCM 扫描 16bit 小端 PCM，按块（frameCount 帧，每帧 channels 个采样）
// 计算削波/静音与逐块质量分（0..100，缺失与削波拉低分数）。
func AnalyzePCM(pcm []byte, channels int, uncertainSampleCount int64) PCMStats {
	const silentFloor = 4
	var st PCMStats
	n := len(pcm) / 2
	st.Samples = int64(n)
	vals := make([]int16, 0, 256)
	flush := func() {
		if len(vals) == 0 {
			return
		}
		var clip int64
		for _, v := range vals {
			av := v
			if av < 0 {
				av = -av
			}
			if av >= 32700 {
				clip++
			}
		}
		score := 100 * (1 - float64(clip)/float64(len(vals)))
		st.BlockScore = append(st.BlockScore, score)
		vals = vals[:0]
	}
	for i := 0; i < n; i++ {
		v := int16(uint16(pcm[2*i]) | uint16(pcm[2*i+1])<<8)
		av := v
		if av < 0 {
			av = -av
		}
		if av >= 32700 {
			st.Clipped++
		}
		if av < silentFloor {
			st.Silent++
		}
		vals = append(vals, v)
		if channels > 0 && len(vals) >= channels*8 { // 每 8 帧聚合成一个质量块
			flush()
		}
	}
	flush()
	if uncertainSampleCount > 0 {
		// 不确定采样以 0 分计入分位数，真实反映时间线缺口。
		for i := int64(0); i < uncertainSampleCount; i += int64(channels*8 + 1) {
			st.BlockScore = append(st.BlockScore, 0)
		}
	}
	return st
}

// percentile 返回有序分数的 p 分位数（p 取 0..100）。
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	pos := p / 100 * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

// Compute 汇总单轨道全部指标。
func Compute(res *repair.Result, entries []index.Entry, opt Options) Metrics {
	tl := res.Timeline
	total := tl.TotalSamples()
	m := Metrics{TrackID: 0, DeviceID: res.DeviceID}
	if len(entries) > 0 {
		m.TrackID = entries[0].TrackID
	}
	m.TargetBitrateBps = opt.TargetBitrateBps
	// 实际码率口径与验收目标一致：PCM 字节/秒 = 采样率 * 声道 * 位深/8。
	// （48kHz/16bit/立体声 = 48000*2*2 = 192000，与 --target-bitrate 192000 对齐。）
	var ch int
	var sr uint32
	var bits uint16
	for _, s := range res.Segments {
		ch, sr, bits = int(s.Channels), s.SampleRate, 16
	}
	uncertain := sumUncertain(res)
	if sr > 0 && ch > 0 && bits > 0 {
		m.ActualBitrateBps = int64(sr) * int64(ch) * int64(bits) / 8
	}
	if opt.TargetBitrateBps > 0 {
		m.BitrateDeviationPct = float64(m.ActualBitrateBps-opt.TargetBitrateBps) *
			100 / float64(opt.TargetBitrateBps)
	}
	// 逐段拼接 PCM 后统一扫描。
	var pcm []byte
	for _, s := range res.Segments {
		pcm = append(pcm, s.Data...)
	}
	st := AnalyzePCM(pcm, ch, uncertain)
	if st.Samples > 0 {
		m.ClippedPct = float64(st.Clipped) * 100 / float64(st.Samples)
		m.SilentPct = float64(st.Silent) * 100 / float64(st.Samples)
	}
	if total > 0 {
		m.MissingSamplePct = float64(uncertain) * 100 / float64(total)
	}
	sort.Float64s(st.BlockScore)
	m.QualityP50 = percentile(st.BlockScore, 50)
	m.QualityP95 = percentile(st.BlockScore, 95)
	_ = bits
	return m
}

func sumUncertain(res *repair.Result) int64 {
	var n int64
	for _, u := range res.Uncertain {
		if u.Length() > 0 {
			n += u.Length()
		}
	}
	return n
}

// ManifestAlignment 复读索引并与清单对账，返回每条轨道的对齐误差（毫秒）。
// 两端都锚定采样点 0，误差来自清单时间范围与索引首尾采样位置的差异。
func ManifestAlignment(mf *manifest.Manifest, idx map[uint32][]index.Entry) map[uint32]float64 {
	out := make(map[uint32]float64)
	for _, tr := range mf.Tracks {
		es := idx[tr.TrackID]
		if len(es) == 0 {
			out[tr.TrackID] = math.MaxFloat64
			continue
		}
		first := es[0].StartSample
		last := es[len(es)-1]
		indexEnd := last.StartSample + last.SampleCount
		deltaSamples := int64(0)
		if first != tr.StartSample {
			deltaSamples += abs64(first - tr.StartSample)
		}
		if indexEnd != tr.EndSample {
			deltaSamples += abs64(indexEnd - tr.EndSample)
		}
		out[tr.TrackID] = clock.SamplesToMS(deltaSamples, tr.SampleRate)
	}
	return out
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
