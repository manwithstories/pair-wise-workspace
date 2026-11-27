package audio

import "math"

func float32Bits(f float32) uint32  { return math.Float32bits(f) }
func float32FromBits(b uint32) float32 { return math.Float32frombits(b) }

// Silence 生成长度为 frames 的交错立体声静音。
// 用于突发丢失占位区间：明确标记为缺失，绝不与解码音频混淆。
func Silence(frames int) *Buffer {
	return &Buffer{Samples: make([]float32, frames*2), Frames: frames}
}

// Append 拼接两个缓冲，返回新缓冲，保持交错立体声。
func Append(a, b *Buffer) *Buffer {
	out := &Buffer{Frames: a.Frames + b.Frames}
	out.Samples = make([]float32, 0, len(a.Samples)+len(b.Samples))
	out.Samples = append(out.Samples, a.Samples...)
	out.Samples = append(out.Samples, b.Samples...)
	return out
}

// Accumulator 是预分配的可增长交错立体声缓冲，避免逐帧 Append 的重复扩容与拷贝。
type Accumulator struct {
	Samples []float32
	Frames  int
}

// NewAccumulator 按预估总帧数一次性分配后备数组。
func NewAccumulator(estimateFrames int) *Accumulator {
	if estimateFrames < 0 {
		estimateFrames = 0
	}
	return &Accumulator{Samples: make([]float32, 0, estimateFrames*2)}
}

// Write 追加一个缓冲的样本（零额外分配，容量不足时仅触发一次增长）。
func (a *Accumulator) Write(b *Buffer) {
	if b == nil || b.Frames == 0 {
		return
	}
	a.Samples = append(a.Samples, b.Samples...)
	a.Frames += b.Frames
}

// WriteSilence 追加 frames 帧静音。
func (a *Accumulator) WriteSilence(frames int) {
	if frames <= 0 {
		return
	}
	start := len(a.Samples)
	a.Samples = append(a.Samples, make([]float32, frames*2)...)
	a.Frames += frames
	// 显式清零（新 append 的 make 本就为零，保留语义清晰）。
	for i := start; i < len(a.Samples); i++ {
		a.Samples[i] = 0
	}
}

// Buffer 转成不可再增长的定长缓冲视图，共享后备数组。
func (a *Accumulator) Buffer() *Buffer {
	return &Buffer{Samples: a.Samples, Frames: a.Frames}
}

