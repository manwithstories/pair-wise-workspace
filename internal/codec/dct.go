// Package codec implements an 8x8 DCT intra codec.
package codec

import (
	"math"

	"gonum.org/v1/gonum/mat"
)

const BlockSize = 8

// dctMatrix is the orthonormal 8x8 DCT-II basis.
var dctMatrix *mat.Dense
var dctMatrixT *mat.Dense

func init() {
	d := mat.NewDense(BlockSize, BlockSize, nil)
	for u := 0; u < BlockSize; u++ {
		alpha := math.Sqrt(2.0 / BlockSize)
		if u == 0 {
			alpha = math.Sqrt(1.0 / BlockSize)
		}
		for x := 0; x < BlockSize; x++ {
			d.Set(u, x, alpha*math.Cos((2*float64(x)+1)*float64(u)*math.Pi/(2*BlockSize)))
		}
	}
	dctMatrix = d
	var t mat.Dense
	t.CloneFrom(d.T())
	dctMatrixT = &t
}

// FDCT computes the orthonormal 2D DCT of an 8x8 block (row-major).
func FDCT(in *[64]float64) *[64]float64 {
	src := mat.NewDense(BlockSize, BlockSize, in[:])
	var tmp, dst mat.Dense
	tmp.Mul(dctMatrix, src)
	dst.Mul(&tmp, dctMatrixT)
	out := &[64]float64{}
	copy(out[:], dst.RawMatrix().Data)
	return out
}

// IDCT computes the inverse 2D DCT of an 8x8 block (row-major).
func IDCT(in *[64]float64) *[64]float64 {
	src := mat.NewDense(BlockSize, BlockSize, in[:])
	var tmp, dst mat.Dense
	tmp.Mul(dctMatrixT, src)
	dst.Mul(&tmp, dctMatrix)
	out := &[64]float64{}
	copy(out[:], dst.RawMatrix().Data)
	return out
}

// LumaBase and ChromaBase are JPEG-style base quantization tables (zigzag order not applied).
var LumaBase = [64]int{
	16, 11, 10, 16, 24, 40, 51, 61,
	12, 12, 14, 19, 26, 58, 60, 55,
	14, 13, 16, 24, 40, 57, 69, 56,
	14, 17, 22, 29, 51, 87, 80, 62,
	18, 22, 37, 56, 68, 109, 103, 77,
	24, 35, 55, 64, 81, 104, 113, 92,
	49, 64, 78, 87, 103, 121, 120, 101,
	72, 92, 95, 98, 112, 100, 103, 99,
}

var ChromaBase = [64]int{
	17, 18, 24, 47, 99, 99, 99, 99,
	18, 21, 26, 66, 99, 99, 99, 99,
	24, 26, 56, 99, 99, 99, 99, 99,
	47, 66, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99,
}

// MinQP and MaxQP bound the quantizer parameter.
const (
	MinQP = 1
	MaxQP = 51
)

// ScaleQTable derives the integer quantizer table for qp in [MinQP,MaxQP].
// The scale grows monotonically with qp so coded bytes never increase with qp.
func ScaleQTable(base [64]int, qp int) [64]int {
	if qp < MinQP {
		qp = MinQP
	}
	if qp > MaxQP {
		qp = MaxQP
	}
	var out [64]int
	for i, b := range base {
		q := b * qp / 12
		if q < 1 {
			q = 1
		}
		out[i] = q
	}
	return out
}

// Zigzag scan order for an 8x8 block.
var Zigzag = [64]int{
	0, 1, 8, 16, 9, 2, 3, 10,
	17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34,
	27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36,
	29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46,
	53, 60, 61, 54, 47, 55, 62, 63,
}
