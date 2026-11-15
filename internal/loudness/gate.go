package loudness

import (
	"math"
)

const (
	absoluteGateLUFS = -70.0
	relativeGateLU   = -10.0
)

// gateBlocks performs the two-pass BS.1770 gate. First blocks below -70 LUFS
// are removed; the relative threshold is then computed from their mean and
// blocks more than 10 LU below that mean are removed.
func gateBlocks(blocks []float64) (integrated float64, gated int, reason string) {
	absEnergy := math.Pow(10, (absoluteGateLUFS+0.691)/10)
	sum := 0.0
	count := 0
	for _, z := range blocks {
		if z > absEnergy {
			sum += z
			count++
		}
	}
	if count == 0 {
		if len(blocks) > 0 && allZero(blocks) {
			return math.Inf(-1), 0, ReasonSilent
		}
		return math.Inf(-1), 0, ReasonBelowAbsoluteGate
	}

	relativeEnergy := (sum / float64(count)) * math.Pow(10, relativeGateLU/10)
	sum = 0
	gated = 0
	for _, z := range blocks {
		if z > absEnergy && z > relativeEnergy {
			sum += z
			gated++
		}
	}
	if gated == 0 {
		return math.Inf(-1), 0, ReasonNoGatedBlocks
	}
	return -0.691 + 10*math.Log10(sum/float64(gated)), gated, ""
}

func allZero(values []float64) bool {
	for _, v := range values {
		if v != 0 {
			return false
		}
	}
	return true
}
