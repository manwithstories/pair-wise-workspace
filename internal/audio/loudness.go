package audio

import (
	"math"

	"mediaverify/internal/fault"
)

// silenceThresholdDB 静音判定门限（dBFS）。
const silenceThresholdDB = -60.0

// PeakLimitDBFS 输出峰值上限。
const PeakLimitDBFS = -1.0

// LoudnessStats 响度/峰值/静音统计。
type LoudnessStats struct {
	LUFS         float64 `json:"lufs"`
	PeakDBFS     float64 `json:"peak_dbfs"`
	SilenceRatio float64 `json:"silence_ratio"`
	Samples      int64   `json:"samples"`
}

// silenceWindow 静音判定窗（采样数，48kHz 下 1ms）。
const silenceWindow = 48

// Meter 流式响度计：累计能量、峰值与静音采样数，不缓存整节目。
type Meter struct {
	sumSquares float64
	peak       float64
	silent     int64
	total      int64
	winPeak    float64
	winCount   int
}

// Add 喂入一个交错采样块。静音按短窗峰值判定，避免过零点误判。
func (m *Meter) Add(samples []float64) {
	th := math.Pow(10, silenceThresholdDB/20)
	for _, s := range samples {
		a := math.Abs(s)
		m.sumSquares += s * s
		if a > m.peak {
			m.peak = a
		}
		if a > m.winPeak {
			m.winPeak = a
		}
		m.winCount++
		if m.winCount == silenceWindow {
			if m.winPeak < th {
				m.silent += int64(silenceWindow)
			}
			m.winPeak, m.winCount = 0, 0
		}
		m.total++
	}
}

// Stats 返回当前统计。LUFS 采用 RMS 近似（K 计权简化为 -0.691 偏移）。
func (m *Meter) Stats() LoudnessStats {
	st := LoudnessStats{Samples: m.total, PeakDBFS: -math.Inf(1), LUFS: -math.Inf(1)}
	if m.total == 0 {
		return st
	}
	rms := math.Sqrt(m.sumSquares / float64(m.total))
	if rms > 0 {
		st.LUFS = -0.691 + 20*math.Log10(rms)
	}
	if m.peak > 0 {
		st.PeakDBFS = 20 * math.Log10(m.peak)
	}
	// 尾部不足一窗的采样也参与静音占比
	silent := m.silent
	if m.winCount > 0 && m.winPeak < math.Pow(10, silenceThresholdDB/20) {
		silent += int64(m.winCount)
	}
	st.SilenceRatio = float64(silent) / float64(m.total)
	return st
}

// GainToTarget 计算把 inputLUFS 拉到 targetLUFS 所需增益（dB）。
func GainToTarget(inputLUFS, targetLUFS float64) float64 {
	return targetLUFS - inputLUFS
}

// ApplyGain 对流式块施加增益（dB），就地修改。
func ApplyGain(samples []float64, gainDB float64) {
	g := math.Pow(10, gainDB/20)
	for i := range samples {
		samples[i] *= g
	}
}

// GainError 增益后峰值超限时返回的错误，保留质量统计。
type GainError struct {
	Err   *fault.Error
	Stats LoudnessStats
}

func (e *GainError) Error() string { return e.Err.Error() }

// Fault 返回底层可定位错误。
func (e *GainError) Fault() *fault.Error { return e.Err }

// CheckPeak 校验施加 gainDB 后的峰值是否超限；超限返回保留统计的错误。
func CheckPeak(in LoudnessStats, gainDB float64) error {
	outPeak := in.PeakDBFS + gainDB
	if outPeak > PeakLimitDBFS {
		return &GainError{
			Err: fault.New(fault.KindLoudness, fault.Fail, "audio", 0, -1,
				"peak %.2f dBFS would exceed limit %.2f after %.2f dB gain",
				outPeak, PeakLimitDBFS, gainDB),
			Stats: LoudnessStats{
				LUFS:         in.LUFS + gainDB,
				PeakDBFS:     outPeak,
				SilenceRatio: in.SilenceRatio,
				Samples:      in.Samples,
			},
		}
	}
	return nil
}
