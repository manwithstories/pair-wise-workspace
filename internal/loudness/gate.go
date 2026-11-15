package loudness

import "math"

const (
	AbsoluteGateLUFS = -70.0
	RelativeOffsetLU = 10.0
	IntegratedOffset = -0.691
)

type ShortTerm struct {
	MaxLUFS   float64 `json:"max_lufs"`
	StartMs   int64   `json:"start_ms"`
	Partial   bool    `json:"partial"`
	BlockSpan int     `json:"block_span"`
}

type GateResult struct {
	IntegratedLUFS   float64   `json:"integrated_lufs"`
	Defined          bool      `json:"defined"`
	RelativeGateLUFS float64   `json:"relative_gate_lufs"`
	BlocksTotal      int       `json:"blocks_total"`
	BlocksAbsolute   int       `json:"blocks_after_absolute"`
	BlocksRelative   int       `json:"blocks_after_relative"`
	ShortTerm        ShortTerm `json:"short_term"`
}

func energyAt(lufs float64) float64 {
	return math.Pow(10, (lufs-IntegratedOffset)/10)
}

func lufsFromEnergy(energy float64) float64 {
	return IntegratedOffset + 10*math.Log10(energy)
}

func Compute(blocks []Block) GateResult {
	result := GateResult{
		IntegratedLUFS:   math.NaN(),
		RelativeGateLUFS: math.NaN(),
		BlocksTotal:      len(blocks),
		ShortTerm: ShortTerm{MaxLUFS: math.NaN()},
	}
	absoluteEnergy := energyAt(AbsoluteGateLUFS)
	absoluteBlocks := make([]Block, 0, len(blocks))
	var absoluteSum float64
	for _, block := range blocks {
		if block.Energy > absoluteEnergy {
			absoluteBlocks = append(absoluteBlocks, block)
			absoluteSum += block.Energy
		}
	}
	result.BlocksAbsolute = len(absoluteBlocks)
	if len(absoluteBlocks) == 0 {
		return result
	}
	ungatedMean := absoluteSum / float64(len(absoluteBlocks))
	relativeLUFS := lufsFromEnergy(ungatedMean) - RelativeOffsetLU
	result.RelativeGateLUFS = relativeLUFS
	relativeEnergy := energyAt(relativeLUFS)
	var relativeSum float64
	relativeCount := 0
	for _, block := range absoluteBlocks {
		if block.Energy > relativeEnergy {
			relativeSum += block.Energy
			relativeCount++
		}
	}
	if relativeCount == 0 {
		return result
	}
	result.BlocksRelative = relativeCount
	result.IntegratedLUFS = lufsFromEnergy(relativeSum / float64(relativeCount))
	result.Defined = true
	windowHops := int(math.Round(ShortWindow / HopSeconds))
	var bestEnergy float64
	var bestStart int64
	for start := 0; start+windowHops <= len(blocks); start++ {
		var sum float64
		for i := start; i < start+windowHops; i++ {
			sum += blocks[i].Energy
		}
		mean := sum / float64(windowHops)
		if mean > bestEnergy || start == 0 {
			bestEnergy = mean
			bestStart = blocks[start].StartMs
		}
	}
	if bestEnergy > 0 {
		result.ShortTerm.MaxLUFS = lufsFromEnergy(bestEnergy)
		result.ShortTerm.StartMs = bestStart
		result.ShortTerm.BlockSpan = windowHops
		return result
	}
	var sum float64
	for _, block := range blocks {
		sum += block.Energy
	}
	result.ShortTerm.MaxLUFS = lufsFromEnergy(sum / float64(len(blocks)))
	result.ShortTerm.StartMs = blocks[0].StartMs
	result.ShortTerm.Partial = true
	result.ShortTerm.BlockSpan = len(blocks)
	return result
}
