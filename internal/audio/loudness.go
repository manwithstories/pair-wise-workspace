package audio

import (
	"math"
)

// MaxGainDB is the hard ceiling applied to quiet material so normalization
// can never amplify noise without bound.
const MaxGainDB = 12.0

// Normalizer measures block loudness and applies a capped gain toward a
// target, with a final limiter that prevents clipping.
type Normalizer struct {
	targetLUFS float64
	maxGainDB  float64
}

// NewNormalizer builds a normalizer for the given integrated target LUFS.
func NewNormalizer(targetLUFS float64) *Normalizer {
	return &Normalizer{targetLUFS: targetLUFS, maxGainDB: MaxGainDB}
}

// MeasureLUFS returns an RMS-based LUFS estimate for an interleaved block.
// A silent block reports -70 LUFS (the practical silence floor).
func MeasureLUFS(samples []int16) float64 {
	if len(samples) == 0 {
		return -70
	}
	var sumSq float64
	for _, s := range samples {
		v := float64(s) / 32768.0
		sumSq += v * v
	}
	rms := math.Sqrt(sumSq / float64(len(samples)))
	if rms < 1e-10 {
		return -70
	}
	return 20 * math.Log10(rms)
}

// GainDB returns the gain needed to move measured loudness to the target,
// clamped to MaxGainDB. Attenuation is unlimited.
func (n *Normalizer) GainDB(measured float64) float64 {
	gain := n.targetLUFS - measured
	if gain > n.maxGainDB {
		gain = n.maxGainDB
	}
	return gain
}

// Process applies the capped target gain and a hard limiter. The limiter
// guarantees no sample exceeds the int16 range.
func (n *Normalizer) Process(samples []int16) []int16 {
	measured := MeasureLUFS(samples)
	gainDB := n.GainDB(measured)
	linear := math.Pow(10, gainDB/20)
	out := make([]int16, len(samples))
	for i, s := range samples {
		scaled := float64(s) * linear
		out[i] = clampInt16(scaled)
	}
	return out
}

// ApplyGain is a stateless helper used by tests and the pipeline when the
// gain decision is made outside the normalizer.
func ApplyGain(samples []int16, gainDB float64) []int16 {
	linear := math.Pow(10, gainDB/20)
	out := make([]int16, len(samples))
	for i, s := range samples {
		out[i] = clampInt16(float64(s) * linear)
	}
	return out
}
