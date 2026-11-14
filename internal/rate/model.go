// Package rate models the bits/QP relation and picks quantizers.
package rate

import (
	"math"

	"gonum.org/v1/gonum/stat"
)

// point is one observed (qp, bits) sample in log-log space.
type point struct {
	logQP   float64
	logBits float64
}

// Model fits log(bits) = alpha + beta*log(qp) over recent segments.
type Model struct {
	points []point
}

// NewModel creates an empty model.
func NewModel() *Model { return &Model{} }

// Add records an observation.
func (m *Model) Add(qp int, bits int64) {
	if qp < 1 || bits < 1 {
		return
	}
	m.points = append(m.points, point{logQP: math.Log(float64(qp)), logBits: math.Log(float64(bits))})
}

// Observations returns how many samples are stored.
func (m *Model) Observations() int { return len(m.points) }

// Last returns the most recent observation.
func (m *Model) Last() (qp int, bits int64, ok bool) {
	if len(m.points) == 0 {
		return 0, 0, false
	}
	p := m.points[len(m.points)-1]
	return int(math.Round(math.Exp(p.logQP))), int64(math.Round(math.Exp(p.logBits))), true
}

// Fit regresses over the most recent k observations.
// ok is false when data is insufficient or the slope is untrustworthy.
func (m *Model) Fit(k int) (alpha, beta float64, ok bool) {
	n := len(m.points)
	if n < 2 {
		return 0, 0, false
	}
	if k > 0 && n > k {
		n = k
	}
	pts := m.points[len(m.points)-n:]
	xs := make([]float64, len(pts))
	ys := make([]float64, len(pts))
	for i, p := range pts {
		xs[i] = p.logQP
		ys[i] = p.logBits
	}
	alpha, beta = stat.LinearRegression(xs, ys, nil, false)
	if math.IsNaN(alpha) || math.IsNaN(beta) {
		return 0, 0, false
	}
	// bits must decrease as qp grows; reject flat or inverted slopes
	if beta > -0.05 {
		return 0, 0, false
	}
	return alpha, beta, true
}

// PredictQP inverts the fit: which qp yields targetBits.
func PredictQP(alpha, beta float64, targetBits int64) float64 {
	if targetBits < 1 {
		return 1
	}
	logQP := (math.Log(float64(targetBits)) - alpha) / beta
	return math.Exp(logQP)
}
