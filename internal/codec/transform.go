// Package codec implements 8x8 DCT, quantization, entropy coding,
// and a small self-contained segment container format.
package codec

import "math"

// QP bounds for the encoder search.
const (
	QPMin = 1
	QPMax = 127
)

var (
	cosT   [8][8]float64
	zigzag [64]int
)

func init() {
	for u := 0; u < 8; u++ {
		for x := 0; x < 8; x++ {
			cosT[u][x] = math.Cos(float64(2*x+1) * float64(u) * math.Pi / 16)
		}
	}
	// Zigzag scan order over an 8x8 matrix in row-major layout.
	x, y := 0, 0
	for i := 0; i < 64; i++ {
		zigzag[i] = y*8 + x
		if (x+y)%2 == 0 { // moving up-right
			if x == 7 {
				y++
			} else if y == 0 {
				x++
			} else {
				x++
				y--
			}
		} else { // moving down-left
			if y == 7 {
				x++
			} else if x == 0 {
				y++
			} else {
				x--
				y++
			}
		}
	}
}

// dct8x8 computes the orthonormal 2D DCT-II of an 8x8 block.
func dct8x8(in, out *[64]float64) {
	var tmp [64]float64
	for r := 0; r < 8; r++ {
		for u := 0; u < 8; u++ {
			sum := 0.0
			for x := 0; x < 8; x++ {
				sum += in[r*8+x] * cosT[u][x]
			}
			tmp[r*8+u] = sum
		}
	}
	for v := 0; v < 8; v++ {
		for u := 0; u < 8; u++ {
			sum := 0.0
			for r := 0; r < 8; r++ {
				sum += tmp[r*8+u] * cosT[v][r]
			}
			cu, cv := 1.0, 1.0
			if u == 0 {
				cu = 1 / math.Sqrt2
			}
			if v == 0 {
				cv = 1 / math.Sqrt2
			}
			out[v*8+u] = 0.25 * cu * cv * sum
		}
	}
}

// idct8x8 inverts dct8x8.
func idct8x8(in, out *[64]float64) {
	var tmp [64]float64
	for v := 0; v < 8; v++ {
		cv := 1.0
		if v == 0 {
			cv = 1 / math.Sqrt2
		}
		for x := 0; x < 8; x++ {
			sum := 0.0
			for u := 0; u < 8; u++ {
				cu := 1.0
				if u == 0 {
					cu = 1 / math.Sqrt2
				}
				sum += cu * in[v*8+u] * cosT[u][x]
			}
			tmp[v*8+x] = 0.25 * cv * sum
		}
	}
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			sum := 0.0
			for v := 0; v < 8; v++ {
				sum += tmp[v*8+x] * cosT[v][y]
			}
			out[y*8+x] = sum
		}
	}
}

// quantizeBlock divides coefficients by the QP step and rounds.
func quantizeBlock(coeffs *[64]float64, qp int, out *[64]int16) {
	step := float64(qp)
	for i, c := range coeffs {
		out[i] = int16(math.Round(c / step))
	}
}

// dequantizeBlock multiplies quantized levels back by the QP step.
func dequantizeBlock(q *[64]int16, qp int, out *[64]float64) {
	step := float64(qp)
	for i, v := range q {
		out[i] = float64(v) * step
	}
}
