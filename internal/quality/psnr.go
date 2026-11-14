// Package quality computes PSNR metrics per frame and per segment.
package quality

import (
	"math"

	"gonum.org/v1/gonum/stat"
)

// MaxPSNR caps the value reported for identical frames.
const MaxPSNR = 99.0

// FramePSNR returns the PSNR in dB between two equal-length plane buffers.
func FramePSNR(a, b []byte) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return MaxPSNR
	}
	var sum float64
	for i := 0; i < n; i++ {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	mse := sum / float64(n)
	if mse == 0 {
		return MaxPSNR
	}
	return 10 * math.Log10(255*255/mse)
}

// MeanStd returns population mean and standard deviation of values.
func MeanStd(values []float64) (float64, float64) {
	if len(values) == 0 {
		return 0, 0
	}
	mean, sampleVar := stat.MeanVariance(values, nil)
	// convert sample variance to population variance
	popVar := sampleVar * float64(len(values)-1) / float64(len(values))
	return mean, math.Sqrt(popVar)
}

// Spread returns max-min of values.
func Spread(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	lo, hi := values[0], values[0]
	for _, v := range values[1:] {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	return hi - lo
}
