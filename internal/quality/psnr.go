// Package quality computes objective frame quality and compares runs.
package quality

import "math"

const perfectPSNR = 99.0

func FramePSNR(reference, candidate []byte) float64 {
	if len(reference) == 0 || len(reference) != len(candidate) {
		return 0
	}
	var sum float64
	for i := range reference {
		d := float64(reference[i]) - float64(candidate[i])
		sum += d * d
	}
	mse := sum / float64(len(reference))
	if mse == 0 {
		return perfectPSNR
	}
	return 10 * math.Log10(255*255/mse)
}

func SegmentPSNR(reference, candidate [][]byte) float64 {
	if len(reference) == 0 || len(reference) != len(candidate) {
		return 0
	}
	sum := 0.0
	for i := range reference {
		sum += FramePSNR(reference[i], candidate[i])
	}
	return sum / float64(len(reference))
}

func Mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func StdDev(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	mean := Mean(values)
	var sum float64
	for _, v := range values {
		d := v - mean
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(values)))
}
