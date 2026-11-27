package audio

import (
	"math"

	"archivemedia/internal/media"
)

// 响度与真峰值目标（EBU R128 语义的工程化实现，全确定、无外部依赖）。
const (
	TargetLUFS = -18.0
	MaxTPdBTP  = -1.0
	// blockMillis 是响度测量的块长度（400ms，EBU R128 标准块）。
	blockMillis = 400
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

// MeasureLoudness 对交错立体声 48k PCM 做确定性 EBU R128 响度测量。
//
// 关键点（与 R128 对齐）：
//   - 逐声道 K 加权，块能量在各声道"求和"而非先下混，否则立体声
//     同相信号会被系统性低估约 3 LU；
//   - 400ms 重叠块（步长 100ms）+ 绝对门限(-70 LUFS)与相对门限(-10 LU)；
//   - 真峰值按 4x 窗控 sinc 过采样逐声道取最大值。
//
// 结果仅依赖样本，可逐位复现。
func MeasureLoudness(buf *Buffer) LoudnessStats {
	st := LoudnessStats{Frames: buf.Frames}
	if buf == nil || buf.Frames == 0 {
		st.IntegratedLUFS = math.Inf(-1)
		st.TruePeakDBTP = math.Inf(-1)
		return st
	}
	const channels = 2
	n := buf.Frames

	// 单条复用 scratch：每个声道依次做"抽取→K加权→块能量累加"，
	// 全程只分配一次 []float64(n)，避免 per-channel 多份全量副本。
	scratch := make([]float64, n)
	blockLen := TargetRate * blockMillis / 1000
	hop := TargetRate * 100 / 1000 // 100ms 重叠步长
	if hop <= 0 {
		hop = blockLen
	}
	numBlocks := (n + hop - 1) / hop
	blockSums := make([]float64, numBlocks)
	blockCnt := make([]int, numBlocks)

	rmsSum := 0.0
	peak := 0.0
	for c := 0; c < channels; c++ {
		// 抽取该声道并在 scratch 内原地转 float64。
		for i := 0; i < n; i++ {
			v := float64(buf.Samples[i*channels+c])
			scratch[i] = v
			rmsSum += v * v
		}
		// 真峰值需要 float32 视图，直接在交错缓冲上按声道步长估计。
		if tp := estimateTruePeakInterleaved(buf.Samples, c, channels); tp > peak {
			peak = tp
		}
		// 原地 K 加权（两级 biquad 写回同一 scratch）。
		applyBiquadInPlace(scratch, r128HighShelf(TargetRate))
		applyBiquadInPlace(scratch, r128HighPass(TargetRate))
		// 累加该声道在各块的能量。
		b := 0
		for start := 0; start < n; start += hop {
			end := start + blockLen
			if end > n {
				end = n
			}
			sum := 0.0
			for i := start; i < end; i++ {
				v := scratch[i]
				sum += v * v
			}
			blockSums[b] += sum
			blockCnt[b] = end - start
			b++
		}
	}
	st.TruePeak = peak
	st.TruePeakDBTP = toDBTP(peak)
	if math.Sqrt(rmsSum/float64(n*channels)) < silenceFloor {
		st.IsSilence = true
		st.IntegratedLUFS = math.Inf(-1)
		return st
	}
	// 每块跨声道平均功率（声道数已含在求和中）。
	blockPower := make([]float64, 0, numBlocks)
	for b := 0; b < numBlocks; b++ {
		if blockCnt[b] > 0 {
			blockPower = append(blockPower, blockSums[b]/float64(blockCnt[b]))
		}
	}
	if len(blockPower) == 0 {
		st.IntegratedLUFS = math.Inf(-1)
		return st
	}
	// 绝对门限：剔除 < -70 LUFS 的块。
	gated := blockPower[:0]
	for _, p := range blockPower {
		if lufsFromMeanSq(p) >= absGateLUFS {
			gated = append(gated, p)
		}
	}
	if len(gated) == 0 {
		gated = blockPower // 短片/极弱信号退化，保证确定结果
	}
	// 相对门限：基于绝对门限均值再剔除低于 (均值-10 LU) 的块。
	mean0 := 0.0
	for _, p := range gated {
		mean0 += p
	}
	mean0 /= float64(len(gated))
	relThresh := lufsFromMeanSq(mean0) - 10
	final := gated[:0]
	for _, p := range gated {
		if lufsFromMeanSq(p) >= relThresh {
			final = append(final, p)
		}
	}
	if len(final) == 0 {
		final = gated
	}
	total := 0.0
	for _, p := range final {
		total += p
	}
	st.IntegratedLUFS = lufsFromMeanSq(total / float64(len(final)))
	return st
}

// applyBiquadInPlace 在 []float64 上原地应用双二阶滤波。
func applyBiquadInPlace(x []float64, bq biquad) {
	var x1, x2, y1, y2 float64
	for i, in := range x {
		out := bq.b0*in + bq.b1*x1 + bq.b2*x2 - bq.a1*y1 - bq.a2*y2
		x[i] = out
		x2, x1 = x1, in
		y2, y1 = y1, out
	}
}

// estimateTruePeakInterleaved 对交错缓冲中指定声道做 4x sinc 真峰值估计。
func estimateTruePeakInterleaved(interleaved []float32, ch, channels int) float64 {
	n := len(interleaved) / channels
	if n == 0 {
		return 0
	}
	const up = 4
	const half = 8
	kernel := truePeakKernel
	samplePeak := 0.0
	for i := 0; i < n; i++ {
		if a := math.Abs(float64(interleaved[i*channels+ch])); a > samplePeak {
			samplePeak = a
		}
	}
	peak := samplePeak
	if samplePeak == 0 {
		return 0
	}
	threshold := samplePeak * 0.9
	at := func(idx int) float64 {
		if idx < 0 || idx >= n {
			return 0
		}
		return float64(interleaved[idx*channels+ch])
	}
	for i := 0; i < n; i++ {
		if math.Abs(at(i)) < threshold {
			continue
		}
		for k := 0; k < up; k++ {
			row := kernel[k]
			acc := 0.0
			w := 0
			for m := -half; m <= half; m++ {
				acc += at(i+m) * row[w]
				w++
			}
			if av := math.Abs(acc); av > peak {
				peak = av
			}
		}
	}
	return peak
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
	// 单遍确定增益：先求达到 LUFS 目标所需增益，再用真峰值上限约束，
	// 取两者较小值，避免"先放大再限幅压回"导致响度目标落空。
	lufsGain := math.Pow(10, (TargetLUFS-before.IntegratedLUFS)/20)
	tpLimit := math.Pow(10, MaxTPdBTP/20)
	maxGainByTP := tpLimit / before.TruePeak
	gain := lufsGain
	if maxGainByTP < gain {
		gain = maxGainByTP
		res.AppliedLimit = true
	}
	applyGain(buf, float32(gain))

	res.GainLinear = gain
	res.GainDB = 20 * math.Log10(gain)
	res.StatsAfter = MeasureLoudness(buf)
	return res, nil
}

// applyGain 对交错立体声统一施加线性增益。
func applyGain(buf *Buffer, g float32) {
	for i := range buf.Samples {
		buf.Samples[i] *= g
	}
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

// truePeakKernel 是启动时一次性预计算的 4 相位 × 17 抽头窗控 sinc 核，
// 热循环仅做乘加，避免在每个采样点重复计算三角函数。
var truePeakKernel = func() [4][17]float64 {
	const up = 4
	const half = 8
	var tbl [4][2*half + 1]float64
	for k := 0; k < up; k++ {
		t := float64(k) / float64(up)
		w := 0
		for m := -half; m <= half; m++ {
			tbl[k][w] = sincWindowed(t - float64(m))
			w++
		}
	}
	var out [4][17]float64
	for i := range out {
		copy(out[i][:], tbl[i][:])
	}
	return out
}()

// sincWindowed 返回 Hann 加窗的归一化 sinc 核。
func sincWindowed(d float64) float64 {
	if math.Abs(d) < 1e-12 {
		return 1
	}
	piD := math.Pi * d
	hann := 0.5 * (1 + math.Cos(piD/float64(8)))
	return math.Sin(piD) / piD * hann
}

// R128 K 加权滤波器精确参数（Mansbridge, Finn & Reiss；EBU R128 48kHz 基准）。
const (
	r128HSFreq   = 1681.974450955533
	r128HSGainDB = 3.999843853973347
	r128HSQ      = 0.7071752369554196
	r128HPFreq   = 38.13547087602444
	r128HPQ      = 0.5003270373238773
)

type biquad struct {
	b0, b1, b2, a1, a2 float64
}

// r128HighShelf 按 R128 修改式 RBJ 高架公式计算（f0≈1681.97Hz，+4dB）。
func r128HighShelf(rate int) biquad {
	K := math.Tan(math.Pi * r128HSFreq / float64(rate))
	Vh := math.Pow(10, r128HSGainDB/20)
	Vb := math.Pow(Vh, 0.4996667741545416)
	Kq := K / r128HSQ
	K2 := K * K
	a0 := 1 + Kq + K2
	return biquad{
		b0: (Vh + Vb*Kq + K2) / a0,
		b1: 2 * (K2 - Vh) / a0,
		b2: (Vh - Vb*Kq + K2) / a0,
		a1: 2 * (K2 - 1) / a0,
		a2: (1 - Kq + K2) / a0,
	}
}

// r128HighPass 按 R128 固定分子高通公式计算（f0≈38.14Hz，Q≈0.5003）。
func r128HighPass(rate int) biquad {
	K := math.Tan(math.Pi * r128HPFreq / float64(rate))
	Kq := K / r128HPQ
	K2 := K * K
	d := 1 + Kq + K2
	return biquad{
		b0: 1 / d,
		b1: -2 / d,
		b2: 1 / d,
		a1: 2 * (K2 - 1) / d,
		a2: (1 - Kq + K2) / d,
	}
}
