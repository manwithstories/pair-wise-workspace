// Package pack 输出封装：以 JPEG 质量参数迭代逼近目标码率。
package pack

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"

	"aircheck/internal/render"
)

// maxIter 二分迭代次数，固定以保证确定性。
const maxIter = 8

// Result 码率控制结果。
type Result struct {
	TargetKbps float64
	ActualKbps float64
	DeltaPct   float64
	Quality    int
	Frames     [][]byte // JPEG 数据
	PSNRMeanDB float64
	PSNRMinDB  float64
}

// RateControl 迭代 JPEG 质量逼近目标码率，输出实际码率、偏差与 PSNR 统计。
// 相同输入必得相同输出（迭代次数与比较规则均确定）。
func RateControl(frames []image.Image, targetKbps, durationMs float64) (*Result, error) {
	if len(frames) == 0 {
		return nil, fmt.Errorf("pack: no frames")
	}
	if targetKbps <= 0 || durationMs <= 0 {
		return nil, fmt.Errorf("pack: invalid target %v kbps over %v ms", targetKbps, durationMs)
	}
	targetBytes := targetKbps * 1000 / 8 * (durationMs / 1000)
	encodeAll := func(q int) ([][]byte, int64, error) {
		out := make([][]byte, len(frames))
		var total int64
		for i, f := range frames {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, f, &jpeg.Options{Quality: q}); err != nil {
				return nil, 0, fmt.Errorf("pack: encode frame %d: %w", i, err)
			}
			out[i] = buf.Bytes()
			total += int64(buf.Len())
		}
		return out, total, nil
	}
	var bestFrames [][]byte
	var bestTotal int64
	bestQ, bestDiff := -1, -1.0
	lo, hi := 1, 100
	for i := 0; i < maxIter && lo <= hi; i++ {
		q := (lo + hi) / 2
		bs, total, err := encodeAll(q)
		if err != nil {
			return nil, err
		}
		diff := absF(float64(total) - targetBytes)
		if bestQ < 0 || diff < bestDiff || (diff == bestDiff && q > bestQ) {
			bestQ, bestDiff, bestFrames, bestTotal = q, diff, bs, total
		}
		if float64(total) > targetBytes {
			hi = q - 1
		} else {
			lo = q + 1
		}
	}
	actualKbps := float64(bestTotal) * 8 / (durationMs / 1000) / 1000
	// 解码回图像计算 PSNR
	packed := make([]image.Image, len(bestFrames))
	for i, bs := range bestFrames {
		img, err := jpeg.Decode(bytes.NewReader(bs))
		if err != nil {
			return nil, fmt.Errorf("pack: decode frame %d: %w", i, err)
		}
		packed[i] = img
	}
	mean, min, err := render.PSNRStats(frames, packed)
	if err != nil {
		return nil, err
	}
	return &Result{
		TargetKbps: targetKbps,
		ActualKbps: actualKbps,
		DeltaPct:   (actualKbps - targetKbps) / targetKbps * 100,
		Quality:    bestQ,
		Frames:     bestFrames,
		PSNRMeanDB: mean,
		PSNRMinDB:  min,
	}, nil
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
