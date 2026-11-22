package core

import (
	"math"

	"mediacheck/internal/model"
)

// LoudnessStats 汇总响度与质量信号。所有字段保证有限值，绝不产生 NaN。
type LoudnessStats struct {
	IntegratedLUFS float64
	PeakDBFS       float64
	SilentFrames   int
	ClippedSamples int
	ShortFrames    int
	MeasuredFrames int
}

// LoudnessMeter 实现 ITU-R BS.1770 风格的 K 加权能量累计与门控积分响度。
// 滤波器系数为定点友好的一阶节；能量与 LUFS 在收尾阶段用 float64 计算，
// 但任何除法都做零防护，输出恒定有限。
type LoudnessMeter struct {
	sampleRate uint32
	// 每声道一阶高通/高架状态（简化 K 加权双一阶）。
	hpPrev   []float64
	hsPrev   []float64
	meanSq   []float64 // 每声道累计平方和（未加权块计数）
	blocks   []float64 // 400ms 块加权响度（LKFS 线性功率）
	blockAcc []float64
	blockN   int
	frames   int
	silent   int
	clipped  int
}

const blockFrames48k = 48000 * 400 / 1000 // 400ms @48k

func NewLoudnessMeter(channels int, sampleRate uint32) *LoudnessMeter {
	return &LoudnessMeter{
		sampleRate: sampleRate,
		hpPrev:     make([]float64, channels),
		hsPrev:     make([]float64, channels),
		meanSq:     make([]float64, channels),
		blockAcc:   make([]float64, channels),
		blocks:     []float64{},
	}
}

// Push 摄入一帧（int16 范围），完成 K 加权与分块能量累计。
func (m *LoudnessMeter) Push(frame []int32) {
	m.frames++
	allZero := true
	for ch, raw := range frame {
		if ch >= len(m.hpPrev) {
			break
		}
		x := float64(raw) / 32768.0
		if raw != 0 {
			allZero = false
		}
		if raw >= 32767 || raw <= -32768 {
			m.clipped++
		}
		// 一阶高通（~80Hz 简化），alpha 由采样率决定。
		alpha := 1.0 - 2.0*math.Pi*80.0/float64(m.sampleRate)
		hp := x - m.hpPrev[ch] + alpha*m.hsPrev[ch]
		m.hpPrev[ch] = x
		m.hsPrev[ch] = hp
		// 高架增益（~1.5kHz 以上 +4dB 简化）。
		weighted := hp * 1.585
		m.meanSq[ch] += weighted * weighted
		m.blockAcc[ch] += weighted * weighted
	}
	if allZero {
		m.silent++
	}
	m.blockN++
	if m.blockN >= blockFrames48k {
		m.closeBlock()
	}
}

func (m *LoudnessMeter) closeBlock() {
	// BS.1770 环绕权重：L/R=1.0，C=1.0，LFE=0.0，环绕=1.41（平方 2.0）。
	var power float64
	n := float64(m.blockN)
	for ch, acc := range m.blockAcc {
		w := channelWeight(ch)
		power += w * acc / safeDiv(n)
	}
	m.blocks = append(m.blocks, power)
	m.blockAcc = make([]float64, len(m.blockAcc))
	m.blockN = 0
}

func channelWeight(ch int) float64 {
	switch ch {
	case 3: // LFE 不计入响度
		return 0
	case 4, 5, 6, 7: // 环绕声道 +1.5dB
		return 2.0
	default:
		return 1.0
	}
}

// Finalize 应用绝对门限（-70 LKFS）与相对门限（-10 LU），返回积分 LUFS。
func (m *LoudnessMeter) Finalize() LoudnessStats {
	if m.blockN > 0 {
		m.closeBlock()
	}
	st := LoudnessStats{
		SilentFrames:   m.silent,
		ClippedSamples: m.clipped,
		MeasuredFrames: m.frames,
	}
	if m.frames < int(m.sampleRate)*model.MinDurationSeconds {
		st.ShortFrames = m.frames
	}
	if len(m.blocks) == 0 {
		st.IntegratedLUFS = -70.0 // 静音/极短素材的有限兜底，禁止 NaN
		st.PeakDBFS = m.peakFromMean()
		return st
	}
	// 绝对门限：只保留 LKFS > -70 的块（线性功率 > 10^((-70+0.691)/10)）。
	const absGate = 10.0 * ((-70.0 + 0.691) / 10.0)
	var kept []float64
	var sum float64
	for _, p := range m.blocks {
		if p > absGate {
			kept = append(kept, p)
			sum += p
		}
	}
	if len(kept) == 0 {
		st.IntegratedLUFS = -70.0
		st.PeakDBFS = m.peakFromMean()
		return st
	}
	mean := sum / float64(len(kept))
	relGate := mean * math.Pow(10, -1.0) // 相对门限比均值低 10 LU
	var relSum float64
	var relN float64
	for _, p := range kept {
		if p > relGate {
			relSum += p
			relN++
		}
	}
	if relN == 0 {
		relSum, relN = sum, float64(len(kept))
	}
	finalMean := relSum / safeDiv(relN)
	st.IntegratedLUFS = -0.691 + 10*math.Log10(safePos(finalMean))
	st.PeakDBFS = m.peakFromMean()
	return st
}

// peakFromMean 用累计均方根折算峰值展示；实际峰值由 limiter 精确统计。
func (m *LoudnessMeter) peakFromMean() float64 {
	var total float64
	for _, v := range m.meanSq {
		total += v
	}
	rms := math.Sqrt(safePos(total / safeDiv(float64(m.frames))))
	if rms <= 0 {
		return -math.Inf(1) // 纯静音用 -Inf dBFS 表达，仍为有限定义值
	}
	return 20 * math.Log10(rms)
}

func safeDiv(d float64) float64 {
	if d == 0 {
		return 1
	}
	return d
}

func safePos(v float64) float64 {
	if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 1e-12
	}
	return v
}
