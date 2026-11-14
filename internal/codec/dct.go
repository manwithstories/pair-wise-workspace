package codec

import "math"

const blockSide = 8

var dctBasis [blockSide][blockSide]float64

func init() {
	for u := 0; u < blockSide; u++ {
		scale := 0.5
		if u == 0 {
			scale = 1 / (2 * math.Sqrt2)
		}
		for x := 0; x < blockSide; x++ {
			dctBasis[u][x] = scale * math.Cos((2*float64(x)+1)*float64(u)*math.Pi/16)
		}
	}
}

// ForwardDCT computes a separable orthonormal 8x8 DCT-II.
func ForwardDCT(src *[64]float64, dst *[64]float64) {
	var tmp [64]float64
	for y := 0; y < blockSide; y++ {
		for u := 0; u < blockSide; u++ {
			sum := 0.0
			for x := 0; x < blockSide; x++ {
				sum += src[y*blockSide+x] * dctBasis[u][x]
			}
			tmp[y*blockSide+u] = sum
		}
	}
	for u := 0; u < blockSide; u++ {
		for v := 0; v < blockSide; v++ {
			sum := 0.0
			for y := 0; y < blockSide; y++ {
				sum += tmp[y*blockSide+u] * dctBasis[v][y]
			}
			dst[v*blockSide+u] = sum
		}
	}
}

// InverseDCT is the orthonormal inverse of ForwardDCT.
func InverseDCT(src *[64]float64, dst *[64]float64) {
	var tmp [64]float64
	for v := 0; v < blockSide; v++ {
		for x := 0; x < blockSide; x++ {
			sum := 0.0
			for u := 0; u < blockSide; u++ {
				sum += src[v*blockSide+u] * dctBasis[u][x]
			}
			tmp[v*blockSide+x] = sum
		}
	}
	for y := 0; y < blockSide; y++ {
		for x := 0; x < blockSide; x++ {
			sum := 0.0
			for v := 0; v < blockSide; v++ {
				sum += tmp[v*blockSide+x] * dctBasis[v][y]
			}
			dst[y*blockSide+x] = sum
		}
	}
}
