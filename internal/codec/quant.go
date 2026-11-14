package codec

import "math"

// QuantStep follows the conventional QP-to-step curve. Integer QPs in
// [18,44] cover the quality range used by the rate allocator.
func QuantStep(qp int) float64 {
	if qp < 0 {
		qp = 0
	}
	if qp > 63 {
		qp = 63
	}
	return 0.625 * math.Exp2(float64(qp)/6)
}

func ClampQP(qp, minQP, maxQP int) int {
	if qp < minQP {
		return minQP
	}
	if qp > maxQP {
		return maxQP
	}
	return qp
}

var zigzag = [64]int{
	0, 1, 8, 16, 9, 2, 3, 10,
	17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34,
	27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36,
	29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46,
	53, 60, 61, 54, 47, 55, 62, 63,
}

// QuantizeZigzag quantizes natural-order coefficients and returns them in
// zigzag scan order, where index zero is DC.
func QuantizeZigzag(coeff *[64]float64, qp int) [64]int32 {
	step := QuantStep(qp)
	var out [64]int32
	for scan, natural := range zigzag {
		out[scan] = int32(math.Round(coeff[natural] / step))
	}
	return out
}

// DequantizeZigzag restores natural-order coefficients from zigzag input.
func DequantizeZigzag(quant [64]int32, qp int) [64]float64 {
	step := QuantStep(qp)
	var out [64]float64
	for scan, natural := range zigzag {
		out[natural] = float64(quant[scan]) * step
	}
	return out
}
