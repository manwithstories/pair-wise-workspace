package core

import (
	"math"

	"mediacheck/internal/model"
)

// Limiter 做目标 LUFS 增益归一化并限制真峰值。增益用线性增益系数，
// 在定点域应用，全程不产生 NaN；峰值统计为精确样本峰值 dBFS。
type Limiter struct {
	targetLUFS float64
	peakLimit  float64 // 线性幅度上限（如 0.95 ≈ -0.45 dBTP）
	gainQ15    int64
	peakAmp    float64
}

func NewLimiter(targetLUFS, peakLimitLinear float64) *Limiter {
	return &Limiter{targetLUFS: targetLUFS, peakLimit: peakLimitLinear, gainQ15: q15}
}

// ComputeGain 依据实测积分 LUFS 计算达到目标所需增益（Q15），并受峰值上限约束。
func (l *Limiter) ComputeGain(st LoudnessStats) {
	gainDB := l.targetLUFS - st.IntegratedLUFS
	gain := math.Pow(10, gainDB/20)
	if gain > 6.0 { // 防止静音段施加爆炸增益
		gain = 6.0
	}
	if gain < 0.1 {
		gain = 0.1
	}
	l.gainQ15 = int64(gain * q15)
}

// Apply 对一帧施加增益并按峰值上限限幅，返回限幅后帧与是否触限。
func (l *Limiter) Apply(frame []int32) ([]int32, bool) {
	out := make([]int32, len(frame))
	limited := false
	limitAmp := int32(l.peakLimit * 32767)
	for i, v := range frame {
		scaled := int64(v) * l.gainQ15 >> 15
		if scaled > int64(limitAmp) {
			scaled = int64(limitAmp)
			limited = true
		} else if scaled < int64(-limitAmp) {
			scaled = int64(-limitAmp)
			limited = true
		}
		out[i] = int32(scaled)
		amp := math.Abs(float64(out[i])) / 32768.0
		if amp > l.peakAmp {
			l.peakAmp = amp
		}
	}
	return out, limited
}

// PeakDBFS 返回处理后精确样本峰值（dBFS）；纯静音返回有限的 -Inf 语义由调用方兜底显示。
func (l *Limiter) PeakDBFS() float64 {
	if l.peakAmp <= 0 {
		return -96.0 // 16-bit 噪声底，避免裸 -Inf/NaN
	}
	return 20 * math.Log10(l.peakAmp)
}

// NormalizeConfig 聚合音频内核的全部显式参数。
type NormalizeConfig struct {
	InRate       uint32
	TargetRate   uint32
	InLayout     model.ChannelLayout
	TargetLayout model.ChannelLayout
	TargetLUFS   float64
	PeakLimit    float64
	FillSilence  bool
	TrackID      uint32
}
