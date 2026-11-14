package codec

import (
	"math"

	"rcx/internal/fixture"
)

// FramePSNR 计算两帧 Y 平面的 PSNR（dB）。完全相同返回 +Inf 前的截断值 99。
func FramePSNR(a, b *fixture.Frame) float64 {
	var sum float64
	for i := range a.Y {
		d := float64(a.Y[i]) - float64(b.Y[i])
		sum += d * d
	}
	if sum == 0 {
		return 99
	}
	mse := sum / float64(len(a.Y))
	return 10 * math.Log10(255*255/mse)
}

// SegmentPSNR 计算逐帧 PSNR。
func SegmentPSNR(src, recon []*fixture.Frame) []float64 {
	out := make([]float64, len(src))
	for i := range src {
		out[i] = FramePSNR(src[i], recon[i])
	}
	return out
}

// MeanStd 返回总体均值与总体标准差。
func MeanStd(xs []float64) (mean, std float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	for _, x := range xs {
		d := x - mean
		std += d * d
	}
	std = math.Sqrt(std / float64(len(xs)))
	return mean, std
}
