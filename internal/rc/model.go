// Package rc 实现码率-量化模型拟合与段预算分配。
package rc

import (
	"math"

	"gonum.org/v1/gonum/stat"
)

// RateModel 描述 R = A · Q^(-B) 的码率-量化关系。
type RateModel struct {
	A float64
	B float64
}

// Fit 用最小二乘在 log-log 域拟合采样点 (Q, R)。
// qs 与 rs 必须等长且至少两个点。
func Fit(qs, rs []float64) RateModel {
	n := len(qs)
	xs := make([]float64, n)
	ys := make([]float64, n)
	for i := 0; i < n; i++ {
		xs[i] = math.Log(qs[i])
		ys[i] = math.Log(rs[i])
	}
	alpha, beta := stat.LinearRegression(xs, ys, nil, false)
	b := -beta
	if b < 0.05 { // 数值保护：曲线过平时给一个保守斜率
		b = 0.05
	}
	return RateModel{A: math.Exp(alpha), B: b}
}

// Predict 估计给定 QP 下的比特数。
func (m RateModel) Predict(q float64) float64 {
	return m.A * math.Pow(q, -m.B)
}

// SolveQP 反解目标比特数对应的 QP（浮点）。
func (m RateModel) SolveQP(targetBits float64) float64 {
	if targetBits <= 0 {
		return 51
	}
	return math.Exp((math.Log(m.A) - math.Log(targetBits)) / m.B)
}
