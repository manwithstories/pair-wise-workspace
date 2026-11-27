package audio

import (
	"encoding/binary"
	"math"

	"github.com/asticode/go-astiav"
)

// 交付响度目标与真峰值上限。
const (
	TargetLUFS = -16.0
	LUFSTol    = 0.5
	// -1 dBTP 对应线性幅度 10^(-1/20)。
	TruePeakLimitLinear = 0.8912509381337456
	// K 加权滤波器系数（EBU R128 / ITU-R BS.1770-4），48kHz 设计。
	// 高架架（shelving）与高通均由双二阶节实现。
	gainAbsMax = 30.0 // 增益安全上限（dB），避免静音段爆炸
)

// LoudnessMeter 实现 EBU R128 简化的 K 加权积分响度测量与增益归一。
// 全程在 48kHz 双声道 fltp 上工作；测量分两遍：先测输入 LUFS 求增益，
// 再施加增益并限制真峰值。引擎在检查点边界按块喂入。
type LoudnessMeter struct {
	// K 加权双二阶状态（每声道独立）。
	hp     [2]biquad // 二阶高通
	shelf  [2]biquad // 高架
	// 累积的加权平方均值（按声道）。
	accSq  [2]float64
	sample int64
}

// biquad 是直接 I 型双二阶滤波器。
type biquad struct {
	b0, b1, b2, a1, a2 float64
	x1, x2, y1, y2    float64
}

func (b *biquad) process(x float64) float64 {
	y := b.b0*x + b.b1*b.x1 + b.b2*b.x2 - b.a1*b.y1 - b.a2*b.y2
	b.x2, b.x1 = b.x1, x
	b.y2, b.y1 = b.y1, y
	return y
}

// NewLoudnessMeter 按 48kHz 初始化 BS.1770 K 加权滤波器。
func NewLoudnessMeter() *LoudnessMeter {
	m := &LoudnessMeter{}
	const fs = 48000.0
	// 高通：f0=38.135Hz, Q=0.5（高切前置）。
	m.hp[0] = makeRBQ(highPass, 38.135/fs, 0.5)
	m.hp[1] = m.hp[0]
	// 高架：f0=1681.976Hz, +4dB。
	m.shelf[0] = makeHighShelf(1681.976/fs, 4.0)
	m.shelf[1] = m.shelf[0]
	return m
}

// Observe 累积一帧 fltp 立体声的加权能量。
func (m *LoudnessMeter) Observe(f *astiav.Frame) {
	n := f.NbSamples()
	for ch := 0; ch < TargetChans; ch++ {
		plane := planeBytes(f, ch)
		samples := bytesToF32(plane)
		for i := 0; i < n && i < len(samples); i++ {
			v := float64(samples[i])
			v = m.hp[ch].process(v)
			v = m.shelf[ch].process(v)
			m.accSq[ch] += v * v
		}
	}
	m.sample += int64(n)
}

// LUFS 返回当前积分响度；无有效能量时返回 -70（近似数字静音）。
func (m *LoudnessMeter) LUFS() float64 {
	if m.sample == 0 {
		return -70
	}
	// BS.1770 声道权重：双声道各 1.0，求和后取均值能量。
	var mean float64
	for ch := 0; ch < TargetChans; ch++ {
		mean += m.accSq[ch] / float64(m.sample)
	}
	if mean <= 1e-12 {
		return -70
	}
	return -0.691 + 10*math.Log10(mean)
}

// GainDB 返回达到目标 LUFS 所需的线性增益（dB），带安全钳制。
func (m *LoudnessMeter) GainDB() float64 {
	g := TargetLUFS - m.LUFS()
	if g > gainAbsMax {
		g = gainAbsMax
	}
	if g < -gainAbsMax {
		g = -gainAbsMax
	}
	return g
}

// ApplyGain 对 fltp 立体声帧施加线性增益并做真峰值软限制。
// 处理后的样本通过 SetBytes 回写到帧；返回限幅触发次数（诊断用）。
func ApplyGain(f *astiav.Frame, gainDB float64) int {
	gain := math.Pow(10, gainDB/20)
	buf, err := f.Data().Bytes(0)
	if err != nil {
		return 0
	}
	clipped := 0
	// Bytes() 经 av_samples_copy_to_buffer 产出紧密排列的平面缓冲，
	// 每平面恰为 NbSamples*4 字节，不使用带对齐填充的 linesize 步进。
	planeBytesN := f.NbSamples() * 4
	for ch := 0; ch < TargetChans; ch++ {
		start := ch * planeBytesN
		if start+planeBytesN > len(buf) {
			break
		}
		plane := bytesToF32(buf[start : start+planeBytesN])
		for i := range plane {
			v := float64(plane[i]) * gain
			// 真峰值限制：超过 -1dBTP 的样本用软拐点压回上限以内。
			if v > TruePeakLimitLinear {
				v = softLimit(v)
				clipped++
			} else if v < -TruePeakLimitLinear {
				v = -softLimit(-v)
				clipped++
			}
			plane[i] = float32(v)
		}
	}
	if err := f.Data().SetBytes(buf, 0); err != nil {
		return clipped
	}
	return clipped
}

// softLimit 把超限样本严格钳制到真峰值上限，保证不越过 -1dBTP。
// 临界点处饱和属于预期行为，由调用方计入限幅诊断。
func softLimit(v float64) float64 {
	if v > TruePeakLimitLinear {
		return TruePeakLimitLinear
	}
	return v
}

// Reset 清空累积状态，供第二遍处理或测试复用。
func (m *LoudnessMeter) Reset() {
	*m = *NewLoudnessMeter()
}

// GainLinear 把 dB 增益换算为线性幅度。
func GainLinear(gainDB float64) float64 {
	return math.Pow(10, gainDB/20)
}

// LimitSample 对单个样本施加真峰值限制，保证 |out| ≤ -1dBTP 线性上限。
func LimitSample(v float64) float64 {
	if v > TruePeakLimitLinear {
		return TruePeakLimitLinear
	}
	if v < -TruePeakLimitLinear {
		return -TruePeakLimitLinear
	}
	return v
}

// FloatToS16 把 [-1,1] 浮点样本饱和转换并以小端写入 dst（需 2 字节）。
func FloatToS16(dst []byte, v float64) {
	binary.LittleEndian.PutUint16(dst, uint16(floatToS16(v)))
}

// floatToS16 返回饱和后的 int16 位模式。
func floatToS16(v float64) int16 {
	switch {
	case v >= 1:
		return math.MaxInt16
	case v <= -1:
		return math.MinInt16
	default:
		return int16(math.Round(v * 32767))
	}
}
