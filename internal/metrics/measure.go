// Package metrics computes bitrate, image quality and alignment error.
package metrics

import (
	"image"
	"math"
	"slices"
)

// MSE returns the mean squared error over the RGB channels of two images
// covering the same rectangle as a.Bounds().
func MSE(a, b image.Image) float64 {
	r := a.Bounds()
	w, h := r.Dx(), r.Dy()
	if w == 0 || h == 0 {
		return 0
	}
	var sum float64
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			ar, ag, ab, _ := a.At(r.Min.X+x, r.Min.Y+y).RGBA()
			br, bg, bb, _ := b.At(r.Min.X+x, r.Min.Y+y).RGBA()
			dr := float64(ar>>8) - float64(br>>8)
			dg := float64(ag>>8) - float64(bg>>8)
			db := float64(ab>>8) - float64(bb>>8)
			sum += dr*dr + dg*dg + db*db
		}
	}
	return sum / float64(w*h*3)
}

// PSNR converts an MSE to decibels; identical images give +Inf.
func PSNR(mse float64) float64 {
	if mse <= 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(255*255/mse)
}

// Median returns the median of values, or 0 for an empty slice.
func Median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// Bitrate computes bits per second from a byte count and a duration.
func Bitrate(bytes int64, seconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	return float64(bytes) * 8 / seconds
}

// DeviationPct returns |actual-target|/target as a percentage.
func DeviationPct(actual, target float64) float64 {
	if target <= 0 {
		return 0
	}
	return math.Abs(actual-target) / target * 100
}

// AlignMaxMs returns the maximum absolute difference between got and want
// PTS positions, converted to milliseconds with secPerTick.
func AlignMaxMs(got, want []int64, secPerTick float64) float64 {
	var max float64
	for i := 0; i < len(got) && i < len(want); i++ {
		d := float64(got[i] - want[i])
		if d < 0 {
			d = -d
		}
		if ms := d * secPerTick * 1000; ms > max {
			max = ms
		}
	}
	return max
}
