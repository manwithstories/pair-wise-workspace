// Package metrics computes per-segment PSNR and spread statistics.
package metrics

import (
	"fmt"
	"math"

	"gonum.org/v1/gonum/stat"
	"tcr/internal/yuv"
)

// SegmentPSNR returns the luma PSNR in dB between two frame sequences.
func SegmentPSNR(orig, dec []*yuv.Frame) (float64, error) {
	if len(orig) == 0 {
		return 0, fmt.Errorf("cannot score zero frames")
	}
	if len(orig) != len(dec) {
		return 0, fmt.Errorf("frame count mismatch: master %d vs decoded %d", len(orig), len(dec))
	}
	var se float64
	var n int
	for i := range orig {
		a, b := orig[i], dec[i]
		if a.W != b.W || a.H != b.H {
			return 0, fmt.Errorf("frame %d dimension mismatch: %dx%d vs %dx%d", i, a.W, a.H, b.W, b.H)
		}
		for j := range a.Y {
			d := float64(a.Y[j]) - float64(b.Y[j])
			se += d * d
		}
		n += len(a.Y)
	}
	mse := se / float64(n)
	if mse == 0 {
		return 99, nil // lossless; cap at a conventional ceiling
	}
	return 10 * math.Log10(255*255/mse), nil
}

// MeanStd returns the mean and sample standard deviation of xs.
func MeanStd(xs []float64) (mean, std float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	return stat.Mean(xs, nil), stat.StdDev(xs, nil)
}
