// Package render 提供离屏合成与质量统计。
package render

import (
	"fmt"
	"image"
	"image/color"
	"math"
)

// MaxPSNRDB 完全相同图像的 PSNR 封顶值，避免统计中出现 Inf。
const MaxPSNRDB = 99.0

// PSNR 计算两幅同尺寸图像的峰值信噪比（dB）。
func PSNR(a, b image.Image) (float64, error) {
	ba, bb := a.Bounds(), b.Bounds()
	if ba.Dx() != bb.Dx() || ba.Dy() != bb.Dy() {
		return 0, fmt.Errorf("render: size mismatch %v vs %v", ba, bb)
	}
	var sum, n float64
	for y := 0; y < ba.Dy(); y++ {
		for x := 0; x < ba.Dx(); x++ {
			r1, g1, b1, _ := a.At(ba.Min.X+x, ba.Min.Y+y).RGBA()
			r2, g2, b2, _ := b.At(ba.Min.X+x, ba.Min.Y+y).RGBA()
			dr := float64(r1>>8) - float64(r2>>8)
			dg := float64(g1>>8) - float64(g2>>8)
			db := float64(b1>>8) - float64(b2>>8)
			sum += dr*dr + dg*dg + db*db
			n += 3
		}
	}
	if sum == 0 {
		return MaxPSNRDB, nil
	}
	mse := sum / n
	return 10 * math.Log10(255*255/mse), nil
}

// PSNRStats 逐对计算 PSNR，返回平均与最低值（dB）。
func PSNRStats(orig, packed []image.Image) (mean, min float64, err error) {
	if len(orig) != len(packed) {
		return 0, 0, fmt.Errorf("render: frame count mismatch %d vs %d", len(orig), len(packed))
	}
	if len(orig) == 0 {
		return 0, 0, fmt.Errorf("render: no frames")
	}
	min = MaxPSNRDB
	for i := range orig {
		p, err := PSNR(orig[i], packed[i])
		if err != nil {
			return 0, 0, err
		}
		mean += p
		if p < min {
			min = p
		}
	}
	return mean / float64(len(orig)), min, nil
}

// BlackFrame 生成纯黑帧。
func BlackFrame(width, height int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	black := color.NRGBA{0, 0, 0, 255}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, black)
		}
	}
	return img
}
