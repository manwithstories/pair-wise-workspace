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
