package media

import "math"

// Quantize16 converts a normalized sample to the PCM16 integer used on the
// wire. The scale is 32768 (not 32767) so the mapping is exactly invertible:
// Dequantize16(Quantize16(s)) rounded back to an integer is stable.
func Quantize16(s float64) int16 {
	if s > 1 {
		s = 1
	}
	if s < -1 {
		s = -1
	}
	v := int(math.Round(s * 32768))
	if v > 32767 {
		v = 32767
	}
	if v < -32768 {
		v = -32768
	}
	return int16(v)
}

// Dequantize16 converts a PCM16 integer back to a normalized sample.
func Dequantize16(q int16) float64 { return float64(q) / 32768 }
