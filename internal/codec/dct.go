// Package codec contains the deterministic baseline video encoder used for
// offline bitrate and quality measurement.
package codec

import (
	"fmt"
	"math"
	"sync"

	"gonum.org/v1/gonum/mat"
)

const BlockSize = 8
const BlockPixels = BlockSize * BlockSize

// Block is a row-major 8x8 sample or transform block.
type Block [BlockPixels]float64

// QuantBlock stores signed quantized coefficients in raster order.
type QuantBlock [BlockPixels]int

var (
	basisOnce sync.Once
	basisMat  *mat.Dense
	basis     [BlockSize][BlockSize]float64
)

// Basis returns the orthonormal 8x8 DCT-II matrix. It is constructed once with
// gonum mat and shared read-only by the transform loops.
func Basis() *mat.Dense {
	basisOnce.Do(func() {
		raw := make([]float64, BlockPixels)
		for u := 0; u < BlockSize; u++ {
			scale := math.Sqrt(2.0 / BlockSize)
			if u == 0 {
				scale = math.Sqrt(1.0 / BlockSize)
			}
			for x := 0; x < BlockSize; x++ {
				raw[u*BlockSize+x] = scale * math.Cos((2*float64(x)+1)*float64(u)*math.Pi/(2*BlockSize))
				basis[u][x] = raw[u*BlockSize+x]
			}
		}
		basisMat = mat.NewDense(BlockSize, BlockSize, raw)
	})
	return basisMat
}

// ForwardDCT transforms an 8x8 sample block into coefficients.
func ForwardDCT(in Block) Block {
	Basis()
	var tmp, out Block
	for u := 0; u < BlockSize; u++ {
		for x := 0; x < BlockSize; x++ {
			var sum float64
			for y := 0; y < BlockSize; y++ {
				sum += basis[u][y] * in[y*BlockSize+x]
			}
			tmp[u*BlockSize+x] = sum
		}
	}
	for u := 0; u < BlockSize; u++ {
		for v := 0; v < BlockSize; v++ {
			var sum float64
			for x := 0; x < BlockSize; x++ {
				sum += tmp[u*BlockSize+x] * basis[v][x]
			}
			out[u*BlockSize+v] = sum
		}
	}
	return out
}

// InverseDCT reconstructs samples from coefficients.
func InverseDCT(in Block) Block {
	Basis()
	var tmp, out Block
	for u := 0; u < BlockSize; u++ {
		for x := 0; x < BlockSize; x++ {
			var sum float64
			for v := 0; v < BlockSize; v++ {
				sum += in[u*BlockSize+v] * basis[v][x]
			}
			tmp[u*BlockSize+x] = sum
		}
	}
	for y := 0; y < BlockSize; y++ {
		for x := 0; x < BlockSize; x++ {
			var sum float64
			for u := 0; u < BlockSize; u++ {
				sum += basis[u][y] * tmp[u*BlockSize+x]
			}
			out[y*BlockSize+x] = sum
		}
	}
	return out
}

// QStep maps QP [1,51] to a positive scalar quantizer step.
func QStep(qp int) float64 {
	qp = clampQP(qp)
	return math.Pow(2.0, float64(qp-4)/6.0)
}

// Quantize applies a dead-zone scalar quantizer to DCT coefficients.
func Quantize(in Block, qp int) QuantBlock {
	step := QStep(qp)
	var out QuantBlock
	for i, v := range in {
		if i == 0 {
			out[i] = clampCoeff(math.Round(v / step))
			continue
		}
		// The dead zone suppresses low-energy AC noise, particularly grain.
		threshold := step * 0.55
		if math.Abs(v) < threshold {
			out[i] = 0
		} else {
			out[i] = clampCoeff(math.Round(v / step))
		}
	}
	return out
}

// Dequantize restores transform-domain values for reconstruction.
func Dequantize(in QuantBlock, qp int) Block {
	step := QStep(qp)
	var out Block
	for i, v := range in {
		out[i] = float64(v) * step
	}
	return out
}

func clampCoeff(v float64) int {
	if v > 32767 {
		return 32767
	}
	if v < -32767 {
		return -32767
	}
	return int(v)
}

func clampQP(qp int) int {
	if qp < 1 {
		return 1
	}
	if qp > 51 {
		return 51
	}
	return qp
}

func clampByte(v float64) byte {
	v = math.Round(v)
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// zigzag maps raster indices to the standard JPEG coefficient order.
var zigzag = [BlockPixels]int{
	0, 1, 8, 16, 9, 2, 3, 10,
	17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34,
	27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36,
	29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46,
	53, 60, 61, 54, 47, 55, 62, 63,
}

// Zigzag returns coefficients in entropy-coding scan order.
func Zigzag(in QuantBlock) [BlockPixels]int {
	var out [BlockPixels]int
	for i, raster := range zigzag {
		out[i] = in[raster]
	}
	return out
}

// Unzigzag reverses Zigzag.
func Unzigzag(in [BlockPixels]int) QuantBlock {
	var out QuantBlock
	for i, raster := range zigzag {
		out[raster] = in[i]
	}
	return out
}

// Sanity validates a block marker used by internal tests.
func Sanity() error {
	if Basis().RawMatrix().Rows != BlockSize || Basis().RawMatrix().Cols != BlockSize {
		return fmt.Errorf("invalid dct basis")
	}
	return nil
}
