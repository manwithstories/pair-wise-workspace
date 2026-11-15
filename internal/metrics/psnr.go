// Package metrics computes luma PSNR and segment quality statistics.
package metrics

import (
	"math"

	"rcctl/internal/source"
)

type Stats struct {
	MeanDB    float64   `json:"mean_db"`
	StdDB     float64   `json:"std_db"`
	MinDB     float64   `json:"min_db"`
	MaxDB     float64   `json:"max_db"`
	FramePSNR []float64 `json:"frame_psnr_db,omitempty"`
}

// FramePSNR compares the luma planes. Identical frames use a 100 dB sentinel.
func FramePSNR(original, reconstructed source.Frame) float64 {
	if original.Width != reconstructed.Width || original.Height != reconstructed.Height ||
		len(original.Y) != len(reconstructed.Y) || len(original.Y) == 0 {
		return 0
	}
	var mse float64
	for i := range original.Y {
		d := float64(original.Y[i]) - float64(reconstructed.Y[i])
		mse += d * d
	}
	mse /= float64(len(original.Y))
	if mse == 0 {
		return 100
	}
	return 10 * math.Log10(255*255/mse)
}

// Compute returns PSNR statistics over paired frames. Mismatched pairs are skipped.
func Compute(original, reconstructed []source.Frame) Stats {
	stats := Stats{}
	n := len(original)
	if len(reconstructed) < n {
		n = len(reconstructed)
	}
	if n == 0 {
		return stats
	}
	stats.MinDB = 100
	var sum float64
	for i := 0; i < n; i++ {
		v := FramePSNR(original[i], reconstructed[i])
		stats.FramePSNR = append(stats.FramePSNR, v)
		sum += v
		if v < stats.MinDB {
			stats.MinDB = v
		}
		if v > stats.MaxDB {
			stats.MaxDB = v
		}
	}
	stats.MeanDB = sum / float64(n)
	for _, v := range stats.FramePSNR {
		d := v - stats.MeanDB
		stats.StdDB += d * d
	}
	stats.StdDB = math.Sqrt(stats.StdDB / float64(n))
	return stats
}

// Aggregate computes mean and population standard deviation across segment means.
func Aggregate(values []float64) (mean, std float64) {
	if len(values) == 0 {
		return 0, 0
	}
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))
	for _, v := range values {
		d := v - mean
		std += d * d
	}
	return mean, math.Sqrt(std / float64(len(values)))
}
