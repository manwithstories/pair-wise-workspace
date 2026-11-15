// Package codec contains a compact DCT-based intra/inter video encoder.
package codec

import (
	"math"

	"gonum.org/v1/gonum/mat"
)

const BlockSize = 8

var dctMatrix [BlockSize][BlockSize]float64

func init() {
	m := mat.NewDense(BlockSize, BlockSize, nil)
	for k := 0; k < BlockSize; k++ {
		alpha := 0.5
		if k == 0 {
			alpha = math.Sqrt(1.0 / BlockSize)
		}
		for n := 0; n < BlockSize; n++ {
			m.Set(k, n, alpha*math.Cos(float64(2*n+1)*float64(k)*math.Pi/float64(2*BlockSize)))
		}
	}
	for k := 0; k < BlockSize; k++ {
		for n := 0; n < BlockSize; n++ {
			dctMatrix[k][n] = m.At(k, n)
		}
	}
}

// DCTMatrix returns a copy of the orthonormal 8x8 DCT basis matrix.
func DCTMatrix() *mat.Dense {
	m := mat.NewDense(BlockSize, BlockSize, nil)
	for k := 0; k < BlockSize; k++ {
		for n := 0; n < BlockSize; n++ {
			m.Set(k, n, dctMatrix[k][n])
		}
	}
	return m
}

// Forward performs a separable 2D DCT on an 8x8 block in row-major order.
func Forward(block *[BlockSize * BlockSize]float64) [BlockSize * BlockSize]float64 {
	var tmp, out [BlockSize * BlockSize]float64
	for y := 0; y < BlockSize; y++ {
		for u := 0; u < BlockSize; u++ {
			var sum float64
			for x := 0; x < BlockSize; x++ {
				sum += dctMatrix[u][x] * block[y*BlockSize+x]
			}
			tmp[y*BlockSize+u] = sum
		}
	}
	for v := 0; v < BlockSize; v++ {
		for u := 0; u < BlockSize; u++ {
			var sum float64
			for y := 0; y < BlockSize; y++ {
				sum += dctMatrix[v][y] * tmp[y*BlockSize+u]
			}
			out[v*BlockSize+u] = sum
		}
	}
	return out
}

// Inverse reconstructs an 8x8 block from DCT coefficients.
func Inverse(coeff *[BlockSize * BlockSize]float64) [BlockSize * BlockSize]float64 {
	var tmp, out [BlockSize * BlockSize]float64
	for y := 0; y < BlockSize; y++ {
		for u := 0; u < BlockSize; u++ {
			var sum float64
			for v := 0; v < BlockSize; v++ {
				sum += dctMatrix[v][y] * coeff[v*BlockSize+u]
			}
			tmp[y*BlockSize+u] = sum
		}
	}
	for y := 0; y < BlockSize; y++ {
		for x := 0; x < BlockSize; x++ {
			var sum float64
			for u := 0; u < BlockSize; u++ {
				sum += dctMatrix[u][x] * tmp[y*BlockSize+u]
			}
			out[y*BlockSize+x] = sum
		}
	}
	return out
}

// Quantize rounds DCT coefficients to multiples of q in raster order.
func Quantize(coeff *[BlockSize * BlockSize]float64, q float64) [BlockSize * BlockSize]int32 {
	var out [BlockSize * BlockSize]int32
	if q < 0.5 {
		q = 0.5
	}
	for i := 0; i < BlockSize*BlockSize; i++ {
		out[i] = int32(math.Round(coeff[i] / q))
	}
	return out
}

// Dequantize restores quantized coefficients to the DCT domain.
func Dequantize(qcoeff *[BlockSize * BlockSize]int32, q float64) [BlockSize * BlockSize]float64 {
	var out [BlockSize * BlockSize]float64
	if q < 0.5 {
		q = 0.5
	}
	for i := 0; i < BlockSize*BlockSize; i++ {
		out[i] = float64(qcoeff[i]) * q
	}
	return out
}

// Zigzag is the conventional 8x8 coefficient scan order.
var Zigzag = [BlockSize * BlockSize]int{
	0, 1, 8, 16, 9, 2, 3, 10, 17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34, 27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36, 29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46, 53, 60, 61, 54, 47, 55, 62, 63,
}
