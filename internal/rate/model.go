// Package rate fits bitrate/quality relationships and allocates QPs under a
// total bit budget.
package rate

import (
	"math"

	"gonum.org/v1/gonum/mat"
)

type LogBitsModel struct {
	Intercept    float64 `json:"intercept"`
	Slope        float64 `json:"slope"`
	Samples      int     `json:"samples"`
	Conservative bool    `json:"conservative"`
}

type LinearModel struct {
	Intercept float64 `json:"intercept"`
	Slope     float64 `json:"slope"`
	Samples   int     `json:"samples"`
}

// FitLogBits fits log2(bits) = intercept + slope*qp with a least-squares QR
// solve. A sparse sample set falls back to a deliberately pessimistic curve.
func FitLogBits(qps []int, bits []uint64) LogBitsModel {
	if len(qps) != len(bits) || len(qps) < 2 {
		return fallbackBits(qps, bits)
	}
	x := make([]float64, len(qps))
	y := make([]float64, len(qps))
	minQ, maxQ := qps[0], qps[0]
	var logSum float64
	for i := range qps {
		if bits[i] == 0 {
			return fallbackBits(qps, bits)
		}
		x[i] = float64(qps[i])
		y[i] = math.Log2(float64(bits[i]))
		logSum += y[i]
		if qps[i] < minQ {
			minQ = qps[i]
		}
		if qps[i] > maxQ {
			maxQ = qps[i]
		}
	}
	if minQ == maxQ {
		return fallbackBits(qps, bits)
	}
	slope, intercept, ok := leastSquares(x, y)
	if !ok || !isFinite(slope) || !isFinite(intercept) || slope >= 0 {
		return fallbackBits(qps, bits)
	}
	return LogBitsModel{Intercept: intercept, Slope: slope, Samples: len(qps)}
}

func (m LogBitsModel) PredictBits(qp int) uint64 {
	v := math.Exp2(m.Intercept + m.Slope*float64(qp))
	if !isFinite(v) || v < 1 {
		v = 1
	}
	if v > float64(^uint64(0)>>1) {
		return ^uint64(0) >> 1
	}
	return uint64(math.Round(v))
}

func FitQuality(qps []int, psnrs []float64) LinearModel {
	if len(qps) != len(psnrs) || len(qps) < 2 {
		return fallbackQuality(qps, psnrs)
	}
	x := make([]float64, len(qps))
	minQ, maxQ := qps[0], qps[0]
	for i, q := range qps {
		x[i] = float64(q)
		if q < minQ {
			minQ = q
		}
		if q > maxQ {
			maxQ = q
		}
	}
	if minQ == maxQ {
		return fallbackQuality(qps, psnrs)
	}
	slope, intercept, ok := leastSquares(x, psnrs)
	if !ok || !isFinite(slope) || !isFinite(intercept) || slope >= 0 {
		return fallbackQuality(qps, psnrs)
	}
	return LinearModel{Intercept: intercept, Slope: slope, Samples: len(qps)}
}

func (m LinearModel) PredictPSNR(qp int) float64 {
	return m.Intercept + m.Slope*float64(qp)
}

func (m LinearModel) QPForPSNR(psnr float64, minQP, maxQP int) int {
	if m.Slope >= 0 || !isFinite(m.Slope) {
		return minQP
	}
	q := int(math.Round((psnr - m.Intercept) / m.Slope))
	if q < minQP {
		return minQP
	}
	if q > maxQP {
		return maxQP
	}
	return q
}

func leastSquares(x, y []float64) (slope, intercept float64, ok bool) {
	n := len(x)
	if n != len(y) || n < 2 {
		return 0, 0, false
	}
	design := mat.NewDense(n, 2, nil)
	values := mat.NewVecDense(n, nil)
	for i := 0; i < n; i++ {
		design.Set(i, 0, 1)
		design.Set(i, 1, x[i])
		values.SetVec(i, y[i])
	}
	var qr mat.QR
	qr.Factorize(design)
	var coeff mat.Dense
	if err := qr.SolveTo(&coeff, false, values); err != nil {
		return 0, 0, false
	}
	return coeff.At(1, 0), coeff.At(0, 0), true
}

func fallbackBits(qps []int, bits []uint64) LogBitsModel {
	const slope = -0.30
	mid, value := 32, 1_000_000.0
	if len(bits) > 0 {
		var sum float64
		var qsum int
		for i := range bits {
			if bits[i] > 0 {
				sum += math.Log2(float64(bits[i]))
				qsum += qps[i]
			}
		}
		if sum > 0 {
			mid = qsum / len(bits)
			value = math.Exp2(sum/float64(len(bits)) + 0.15)
		}
	}
	return LogBitsModel{Intercept: math.Log2(value) - slope*float64(mid), Slope: slope, Samples: len(qps), Conservative: true}
}

func fallbackQuality(qps []int, psnrs []float64) LinearModel {
	const slope = -0.35
	if len(psnrs) > 0 {
		var sum, qsum float64
		for i := range psnrs {
			sum += psnrs[i]
			qsum += float64(qps[i])
		}
		mid := qsum / float64(len(psnrs))
		mean := sum / float64(len(psnrs))
		return LinearModel{Intercept: mean - slope*mid, Slope: slope, Samples: len(qps)}
	}
	return LinearModel{Intercept: 45, Slope: slope}
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
