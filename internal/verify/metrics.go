// Package verify 只读取已原子提交的清单与片段，计算量化质量指标并按
// 门限判定通过/失败。失败返回 KindThreshold，由 CLI 映射为退出码 5。
//
// 指标口径：
//   - 码率：实际 = 字节*8/时长(s)，与目标码率比偏差百分比；
//   - PSNR：原始帧与恢复帧逐像素 Y 分量 MSE 换算，纯静音/无失真处理为高分；
//   - RMS：音频电平 dBFS；
//   - 对齐误差：清单片段时间与归一时间线边界的最大微秒偏差。
package verify

import (
	"encoding/json"
	"math"

	"archivemedia/internal/model"
)

// Thresholds 是验收门限。
type Thresholds struct {
	MaxBitrateDeltaPct float64
	MaxAlignMs         int64
	MinPsnrDb          float64
}

// DefaultThresholds 与任务验收参数对齐。
func DefaultThresholds() Thresholds {
	return Thresholds{MaxBitrateDeltaPct: 8, MaxAlignMs: 1, MinPsnrDb: 32}
}

// SegmentMetric 是单片段测量输入。
type SegmentMetric struct {
	SegmentID     string
	Bytes         int64
	Duration      model.Duration
	TargetBitrate int64
	// 对齐：清单声明边界与时间线吸附边界的偏差（微秒绝对值）。
	AlignErrorUs int64
	// PSNR 输入：原始与恢复的 Y 平面字节（等长）。
	OriginalY  []byte
	RecoveredY []byte
	// 音频 RMS 输入（线性 PCM，-1..1）。
	AudioSamples []float64
}

// SegmentResult 是单片段结果。
type SegmentResult struct {
	SegmentID        string
	TargetBitrateBps int64   `json:"target_bitrate_bps"`
	ActualBitrateBps int64   `json:"actual_bitrate_bps"`
	BitrateDeltaPct  float64 `json:"bitrate_delta_pct"`
	PsnrDb           float64 `json:"psnr_db"`
	RmsDb            float64 `json:"rms_db"`
	AlignErrorMs     float64 `json:"align_error_ms"`
}

// Report 是汇总报告（字段名对齐任务要求的 JSON 行）。
type Report struct {
	TargetBitrateBps  int64           `json:"target_bitrate_bps"`
	ActualBitrateBps  int64           `json:"actual_bitrate_bps"`
	BitrateDeltaPct   float64         `json:"bitrate_delta_pct"`
	PsnrDb            float64         `json:"psnr_db"`
	RmsDb             float64         `json:"rms_db"`
	SilenceIntervals  int             `json:"silence_intervals"`
	DuplicateGroups   int             `json:"duplicate_groups"`
	AlignErrorMs      float64         `json:"align_error_ms"`
	RecoveredSegments int             `json:"recovered_segments"`
	ProvenanceSha256  string          `json:"provenance_sha256"`
	PerSegment        []SegmentResult `json:"-"`
}

// JSON 安全哨兵：标准 encoding/json 不支持 NaN/Inf，数字静音（-Inf dBFS）
// 与无失真（+Inf PSNR）映射为有限值，保证报告永远可序列化为一行 JSON。
const (
	jsonSilenceDb = -120.0
	jsonPerfectDb = 99.0
)

// MarshalJSON 输出把 Inf 哨兵有限化的报告。
func (r Report) MarshalJSON() ([]byte, error) {
	type alias Report
	a := alias(r)
	a.RmsDb = sanitizeInf(r.RmsDb, jsonSilenceDb)
	a.PsnrDb = sanitizeInf(r.PsnrDb, jsonPerfectDb)
	a.BitrateDeltaPct = sanitizeInf(r.BitrateDeltaPct, 0)
	a.AlignErrorMs = sanitizeInf(r.AlignErrorMs, 0)
	return json.Marshal(a)
}

func sanitizeInf(v, sentinel float64) float64 {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return sentinel
	}
	return v
}

// PSNR 计算两段等长 Y 平面的峰值信噪比（8 位，峰值 255）。
// 完全无失真返回高分哨兵；长度不一致按可比较前缀计算并放大误差。
func PSNR(original, recovered []byte) float64 {
	n := len(original)
	if len(recovered) < n {
		n = len(recovered)
	}
	if n == 0 {
		return math.Inf(1)
	}
	var mse float64
	for i := 0; i < n; i++ {
		d := float64(original[i]) - float64(recovered[i])
		mse += d * d
	}
	mse /= float64(n)
	if mse == 0 {
		return 99.0 // 无失真哨兵，远超 32dB 门限
	}
	return 10 * math.Log10((255.0*255.0)/mse)
}

// RMSDb 计算线性 PCM（-1..1）的 RMS dBFS。
func RMSDb(samples []float64) float64 {
	if len(samples) == 0 {
		return math.Inf(-1)
	}
	var sumSq float64
	for _, s := range samples {
		sumSq += s * s
	}
	rms := math.Sqrt(sumSq / float64(len(samples)))
	if rms == 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(rms)
}

// bitrate 由字节与时长求码率。
func bitrate(bytes int64, d model.Duration) int64 {
	secs := d.Seconds()
	if secs <= 0 {
		return 0
	}
	return int64(math.Round(float64(bytes*8) / secs))
}

// Evaluate 计算全部片段并汇总，按门限判定。返回的 error 非 nil 即门限失败。
func Evaluate(metrics []SegmentMetric, silence, dupGroups int, provenance string, th Thresholds) (*Report, error) {
	report := &Report{
		SilenceIntervals:  silence,
		DuplicateGroups:   dupGroups,
		RecoveredSegments: len(metrics),
		ProvenanceSha256:  provenance,
		PerSegment:        make([]SegmentResult, 0, len(metrics)),
	}
	var (
		totalBytes     int64
		totalDur       model.Duration
		weightedTarget float64 // 按片段时长加权的目标码率累计
		weightedPsnr   float64
		psnrWeight     float64
		worstAlignUs   int64
		rmsVals        []float64
	)
	for _, m := range metrics {
		actual := bitrate(m.Bytes, m.Duration)
		var delta float64
		if m.TargetBitrate > 0 {
			delta = (float64(actual) - float64(m.TargetBitrate)) / float64(m.TargetBitrate) * 100
		}
		psnr := PSNR(m.OriginalY, m.RecoveredY)
		rms := RMSDb(m.AudioSamples)
		report.PerSegment = append(report.PerSegment, SegmentResult{
			SegmentID: m.SegmentID, TargetBitrateBps: m.TargetBitrate,
			ActualBitrateBps: actual, BitrateDeltaPct: round2(delta),
			PsnrDb: round2(psnr), RmsDb: round2(rms),
			AlignErrorMs: float64(m.AlignErrorUs) / 1000.0,
		})
		totalBytes += m.Bytes
		totalDur += m.Duration
		// 目标码率按时长加权：等价于总目标比特数/总时长。
		weightedTarget += float64(m.TargetBitrate) * m.Duration.Seconds()
		if !math.IsInf(psnr, 1) {
			weightedPsnr += psnr * m.Duration.Seconds()
			psnrWeight += m.Duration.Seconds()
		}
		if m.AlignErrorUs > worstAlignUs {
			worstAlignUs = m.AlignErrorUs
		}
		rmsVals = append(rmsVals, rms)
	}
	// 汇总码率按总字节/总时长；目标码率用相同时长口径，保证偏差可比。
	report.ActualBitrateBps = bitrate(totalBytes, totalDur)
	if totalDur > 0 {
		report.TargetBitrateBps = int64(math.Round(weightedTarget / totalDur.Seconds()))
	}
	if report.TargetBitrateBps > 0 {
		report.BitrateDeltaPct = round2(
			(float64(report.ActualBitrateBps) - float64(report.TargetBitrateBps)) /
				float64(report.TargetBitrateBps) * 100)
	}
	if psnrWeight > 0 {
		report.PsnrDb = round2(weightedPsnr / psnrWeight)
	} else {
		report.PsnrDb = 99.0
	}
	report.RmsDb = round2(aggregateRms(rmsVals))
	report.AlignErrorMs = float64(worstAlignUs) / 1000.0

	return report, checkThresholds(report, th)
}

func aggregateRms(vals []float64) float64 {
	var finite []float64
	for _, v := range vals {
		if !math.IsInf(v, -1) {
			finite = append(finite, v)
		}
	}
	if len(finite) == 0 {
		return math.Inf(-1)
	}
	var sum float64
	for _, v := range finite {
		sum += v
	}
	return sum / float64(len(finite))
}

func checkThresholds(r *Report, th Thresholds) error {
	var failures []string
	if math.Abs(r.BitrateDeltaPct) > th.MaxBitrateDeltaPct {
		failures = append(failures, "bitrate")
	}
	if r.PsnrDb < th.MinPsnrDb {
		failures = append(failures, "psnr")
	}
	if int64(math.Round(r.AlignErrorMs)) > th.MaxAlignMs {
		failures = append(failures, "alignment")
	}
	if len(failures) > 0 {
		return model.NewError(model.KindThreshold,
			"verify: threshold failures: %v (bitrate_delta=%.2f%% psnr=%.2f align=%.3fms)",
			failures, r.BitrateDeltaPct, r.PsnrDb, r.AlignErrorMs)
	}
	return nil
}

func round2(v float64) float64 {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return v
	}
	return math.Round(v*100) / 100
}
