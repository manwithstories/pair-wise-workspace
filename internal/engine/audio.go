package engine

import "bcast/internal/model"

// ApplyGainS16 applies an exact rational gain to signed 16-bit PCM. It never
// uses floating-point accumulation and clamps only at the PCM representation
// boundary.
func ApplyGainS16(samples []int16, gain model.Rational) []int16 {
	if gain.D == 0 {
		gain = model.Rational{N: 1, D: 1}
	}
	out := make([]int16, len(samples))
	for i, s := range samples {
		v := int64(s) * gain.N / gain.D
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		out[i] = int16(v)
	}
	return out
}
