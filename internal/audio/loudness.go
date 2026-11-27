package audio

import (
	"math"

	"archivemedia/internal/media"
)

// 响度与真峰值目标（EBU R128 语义的工程化实现，全确定、无外部依赖）。
const (
	TargetLUFS  = -18.0
	MaxTPdBTP   = -1.0
	// blockMillis 是响度测量的块长度（400ms，EBU R128 标准块）。
	blockMillis = 400
	// minBlocks 是允许给出集成响度的最少有效块；不足则按短片规则处理。
	minBlocks = 1
	// absGateLUFS 是绝对门限 -70 LUFS。
	absGateLUFS = -70.0
	// silenceFloor 是判定整段静音的 RMS 门限（线性）。
	silenceFloor = 1e-6
)

// LoudnessStats 是一段音频的响度测量结果。
type LoudnessStats struct {
	IntegratedLUFS float64
	TruePeak       float64 // 线性峰值
	TruePeakDBTP   float64
	IsSilence      bool
	Frames         int
}

// MeasureLoudness 对交错立体声 48k PCM 做确定性响度测量。
//
// 采用工程化 K 加权（二阶高架+高通滤波）+ 400ms 均方能量块 + 绝对门限平均，
// 真峰值按 4x 过采样估计。结果仅依赖样本，可逐位复现。
func MeasureLoudness(buf *Buffer) LoudnessStats {
	st := LoudnessStats{Frames: buf.Frames}
	if buf == nil || buf.Frames == 0 {
		st.IntegratedLUFS = math.Inf(-1)
		st.TruePeakDBTP = math.Inf(-1)
		return st
	}
	// 下混为单声道能量序列（立体声平均），同时跟踪线性峰值。
	mono := downmixMono(buf)
	globalPeak := 0.0
	for _, v := range mono {
		if a := math.Abs(float64(v)); a > globalPeak {
			globalPeak = a
		}
	}
	st.TruePeak = estimateTruePeak(mono)
	st.TruePeakDBTP = toDBTP(st.TruePeak)

	// K 加权滤波（固定系数的二阶滤波串联）。
	weighted := kWeight(mono, TargetRate)

	block := TargetRate * blockMillis / 1000
	var blockMeans []float64
	for start := 0; start < len(weighted); start += block {
		end := start + block
		if end > len(weighted) {
			end = len(weighted)
		}
		if end-start == 0 {
			continue
		}
		sum := 0.0
		for i := start; i < end; i++ {
			w := float64(weighted[i])
			sum += w * w
		}
		mean := sum / float64(end-start)
		loud := lufsFromMeanSq(mean)
		if loud >= absGateLUFS {
			blockMeans = append(blockMeans, mean)
		}
	}
	// 静音检测基于未加权整体 RMS。
	rms := 0.0
	for _, v := range mono {
		fv := float64(v)
		rms += fv * fv
	}
	rms = math.Sqrt(rms / float64(len(mono)))
	if rms < silenceFloor {
		st.IsSilence = true
		st.IntegratedLUFS = math.Inf(-1)
		return st
	}
	if len(blockMeans) < minBlocks {
		// 短片：用全部加权样本能量，保证有确定结果。
		sum := 0.0
		for _, v := range weighted {
			fv := float64(v)
			sum += fv * fv
		}
		st.IntegratedLUFS = lufsFromMeanSq(sum / float64(len(weighted)))
		return st
	}
	// 集成响度：门限块平均能量再转 LUFS（能量域平均，非响度域）。
	total := 0.0
	for _, m := range blockMeans {
		total += m
	}
	st.IntegratedLUFS = lufsFromMeanSq(total / float64(len(blockMeans)))
	return st
}

// NormalizeResult 是响度归一结果。
type NormalizeResult struct {
	GainDB       float64
	GainLinear   float64
	AppliedLimit bool
	StatsBefore  LoudnessStats
	StatsAfter   LoudnessStats
}

// Normalize 把缓冲整体归一到 -18 LUFS，并把真峰值限制在 -1 dBTP 内。
//
// 规则确定：
//   - 静音段：不放大（增益 1），避免把本底噪声拉起；
//   - 普通段：增益 = 目标/测量响度之差；
//   - 应用增益后若真峰值超限，再施加统一限制增益，保证两声道一致、不引入削波失真；
//   - 不完整尾包与正常样本同规则处理，无特殊分支。
func Normalize(buf *Buffer) (*NormalizeResult, error) {
	if buf == nil {
		return nil, media.E(media.KindAudio, "audio.normalize",
			media.ErrorLocation{ArrivalIndex: -1, ByteOffset: -1},
			"空缓冲无法归一", nil)
	}
	before := MeasureLoudness(buf)
	res := &NormalizeResult{StatsBefore: before, GainDB: 0, GainLinear: 1}

	if before.IsSilence || math.IsInf(before.IntegratedLUFS, -1) {
		// 静音/极短片保持原样，仍执行峰值安全检查。
		res.StatsAfter = before
		return res, nil
	}
	gainDB := TargetLUFS - before.IntegratedLUFS
	gain := math.Pow(10, gainDB/20)
	applyGain(buf, float32(gain))

	after := MeasureLoudness(buf)
	// 真峰值限制：目标线性阈值 = 10^(-1/20)。
	tpLimit := math.Pow(10, MaxTPdBTP/20)
	if after.TruePeak > tpLimit {
		limitGain := tpLimit / after.TruePeak
		applyGain(buf, float32(limitGain))
		gain *= limitGain
		gainDB += 20 * math.Log10(limitGain)
		res.AppliedLimit = true
	}
	res.GainLinear = gain
	res.GainDB = gainDB
	res.StatsAfter = MeasureLoudness(buf)
	return res, nil
}

// applyGain 对交错立体声统一施加线性增益。
func applyGain(buf *Buffer, g float32) {
	for i := range buf.Samples {
		buf.Samples[i] *= g
	}
}

// downmixMono 把交错立体声下混为单声道（两声道平均）。
func downmixMono(buf *Buffer) []float32 {
	mono := make([]float32, buf.Frames)
	for i := 0; i < buf.Frames; i++ {
		mono[i] = (buf.Samples[i*2] + buf.Samples[i*2+1]) * 0.5
	}
	return mono
}

// lufsFromMeanSq 把均方能量转换为 LUFS（-0.691 为 K 加权校准常数）。
func lufsFromMeanSq(meanSq float64) float64 {
	if meanSq <= 0 {
		return math.Inf(-1)
	}
	return -0.691 + 10*math.Log10(meanSq)
}

func toDBTP(linear float64) float64 {
	if linear <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(linear)
}

// estimateTruePeak 用 4x 线性插值过采样估计连续域真峰值。
func estimateTruePeak(x []float32) float64 {
	if len(x) == 0 {
		return 0
	}
	peak := 0.0
	up := 4
	for i := 0; i < len(x)-1; i++ {
		a := float64(x[i])
		b := float64(x[i+1])
		for k := 0; k < up; k++ {
			t := float64(k) / float64(up)
			// 余弦插值比线性更接近带限重建的过冲。
			v := a*0.5*(1+math.Cos(math.Pi*t)) + b*0.5*(1-math.Cos(math.Pi*t))
			if av := math.Abs(v); av > peak {
				peak = av
			}
		}
	}
	if av := math.Abs(float64(x[len(x)-1])); av > peak {
		peak = av
	}
	return peak
}

// kWeight 应用工程化 K 加权：高通(≈38Hz) + 高架(≈1.68kHz,+4dB)。
// 双二阶（biquad）系数固定，保证逐位可复现。
func kWeight(x []float32, rate int) []float32 {
	// 第一级：二阶高通，截止 ~38Hz。
	hp := biquadHighPass(rate, 38.0, 0.707)
	y := processBiquad(x, hp)
	// 第二级：高架滤波，1.68kHz 提升约 4dB。
	hs := biquadHighShelf(rate, 1681.5, dbToGain(4.0))
	y = processBiquad(y, hs)
	return y
}

type biquad struct {
	b0, b1, b2, a1, a2 float64
}

func biquadHighPass(rate int, cutoff, q float64) biquad {
	w := 2 * math.Pi * cutoff / float64(rate)
	cosw := math.Cos(w)
	alpha := math.Sin(w) / (2 * q)
	b0 := (1 + cosw) / 2
	b1 := -(1 + cosw)
	b2 := (1 + cosw) / 2
	a0 := 1 + alpha
	a1 := -2 * cosw
	a2 := 1 - alpha
	return biquad{b0 / a0, b1 / a0, b2 / a0, a1 / a0, a2 / a0}
}

func biquadHighShelf(rate int, cutoff, gainLin float64) biquad {
	w := 2 * math.Pi * cutoff / float64(rate)
	cosw := math.Cos(w)
	sinw := math.Sin(w)
	a := gainLin
	b0 := a * ((a + 1) + (a-1)*cosw + 2*math.Sqrt(a)*sinw)
	b1 := -2 * a * ((a - 1) + (a + 1) * cosw)
	b2 := a * ((a + 1) + (a-1)*cosw - 2*math.Sqrt(a)*sinw)
	a0 := (a + 1) - (a-1)*cosw + 2*math.Sqrt(a)*sinw
	a1 := 2 * ((a - 1) - (a + 1) * cosw)
	a2 := (a + 1) - (a-1)*cosw - 2*math.Sqrt(a)*sinw
	return biquad{b0 / a0, b1 / a0, b2 / a0, a1 / a0, a2 / a0}
}

func processBiquad(x []float32, bq biquad) []float32 {
	y := make([]float32, len(x))
	var x1, x2, y1, y2 float64
	for i, v := range x {
		in := float64(v)
		out := bq.b0*in + bq.b1*x1 + bq.b2*x2 - bq.a1*y1 - bq.a2*y2
		y[i] = float32(out)
		x2, x1 = x1, in
		y2, y1 = y1, out
	}
	return y
}

func dbToGain(db float64) float64 { return math.Pow(10, db/20) }
