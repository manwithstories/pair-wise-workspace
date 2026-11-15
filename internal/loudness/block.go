package loudness

import "math"

const (
	BlockSeconds = 0.4
	HopSeconds   = 0.1
	OverlapRatio = 0.75
	ShortWindow  = 3.0
)

type Block struct {
	Index   int
	StartMs int64
	Energy  float64
}

type BlockAccumulator struct {
	hopSamples  int
	blockHops   int
	ring        [][]float64
	weights     []float64
	used        []bool
	head        int
	hopsFilled  int
	hopIndex    int
	pending     int
	blockIndex  int
	hopEnergy   float64
}

func NewBlockAccumulator(sampleRate int, weights []float64, used []bool) *BlockAccumulator {
	hop := int(math.Round(float64(sampleRate) * HopSeconds))
	blockHops := int(math.Round(BlockSeconds / HopSeconds))
	ring := make([][]float64, blockHops)
	for i := range ring {
		ring[i] = make([]float64, len(weights))
	}
	return &BlockAccumulator{
		hopSamples: hop,
		blockHops:  blockHops,
		ring:       ring,
		weights:    weights,
		used:       used,
	}
}

func (a *BlockAccumulator) Process(weighted [][]float64) []Block {
	blocks := make([]Block, 0)
	channels := len(weighted)
	for frame := 0; frame < len(weighted[0]); frame++ {
		for ch := 0; ch < channels; ch++ {
			if !a.used[ch] {
				continue
			}
			value := weighted[ch][frame]
			a.hopEnergy += a.weights[ch] * value * value
		}
		a.pending++
		if a.pending == a.hopSamples {
			a.ring[a.head] = make([]float64, channels)
			a.ring[a.head][0] = a.hopEnergy / float64(a.hopSamples)
			a.head = (a.head + 1) % a.blockHops
			if a.hopsFilled < a.blockHops {
				a.hopsFilled++
			}
			a.hopIndex++
			a.hopEnergy = 0
			a.pending = 0
			if a.hopsFilled == a.blockHops {
				var energy float64
				for _, hop := range a.ring {
					energy += hop[0]
				}
				blocks = append(blocks, Block{
					Index:   a.blockIndex,
					StartMs: int64(a.blockIndex) * int64(HopSeconds*1000),
					Energy:  energy / float64(a.blockHops),
				})
				a.blockIndex++
			}
		}
	}
	return blocks
}

func (a *BlockAccumulator) HopCount() int { return a.hopIndex }

func (a *BlockAccumulator) TailPending() int { return a.pending }
