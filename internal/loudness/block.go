// Package loudness contains the streaming block, gating and true-peak
// analysis stages.
package loudness

import (
	"fmt"
	"math"
)

const (
	segmentDuration = 0.1 // seconds
	momentaryBlocks = 4   // 400 ms
	shortTermBlocks = 30  // 3 s
)

const (
	ReasonTooShort          = "too_short"
	ReasonSilent            = "silent"
	ReasonBelowAbsoluteGate = "below_absolute_gate"
	ReasonNoGatedBlocks     = "no_gated_blocks"
)

// Metrics is the measured program loudness and peak summary.
type Metrics struct {
	IntegratedLUFS   float64
	ShortTermMaxLUFS float64
	MomentaryMaxLUFS float64
	TruePeakDBTP     float64
	GatedBlocks      int
	DurationSeconds  float64
	NaNs             uint64
	Reason           string
}

// Analyzer consumes K-weighted interleaved samples. It keeps only 100 ms
// segment energies and the current segment sums, so memory is bounded by the
// number of 400 ms gating blocks rather than by the audio sample count.
type Analyzer struct {
	fs       int
	channels int
	gains    []float64
	segLen   int

	segSums   []float64
	segFrames int
	samples   int64
	segments  int64

	blocks       []float64
	last4        [momentaryBlocks]float64
	last4Pos     int
	momentaryMax float64

	shortRing  [shortTermBlocks]float64
	shortPos   int
	shortCount int
	shortSum   float64
	shortMax   float64
}

func NewAnalyzer(fs, channels int, gains []float64) (*Analyzer, error) {
	a := &Analyzer{}
	if err := a.Reset(fs, channels, gains); err != nil {
		return nil, err
	}
	return a, nil
}

// Reset reuses all block and ring storage for another file.
func (a *Analyzer) Reset(fs, channels int, gains []float64) error {
	if fs <= 0 || channels <= 0 {
		return fmt.Errorf("invalid analyzer format")
	}
	if len(gains) != channels {
		return fmt.Errorf("channel gain count %d, want %d", len(gains), channels)
	}
	segLen := int(math.Round(float64(fs) * segmentDuration))
	if segLen <= 0 {
		segLen = 1
	}
	a.fs, a.channels, a.segLen = fs, channels, segLen
	if cap(a.gains) < channels {
		a.gains = make([]float64, channels)
	} else {
		a.gains = a.gains[:channels]
	}
	copy(a.gains, gains)
	if cap(a.segSums) < channels {
		a.segSums = make([]float64, channels)
	} else {
		a.segSums = a.segSums[:channels]
	}
	for i := range a.segSums {
		a.segSums[i] = 0
	}
	a.blocks = a.blocks[:0]
	a.segFrames = 0
	a.samples = 0
	a.segments = 0
	a.last4 = [momentaryBlocks]float64{}
	a.last4Pos = 0
	a.momentaryMax = math.Inf(-1)
	a.shortRing = [shortTermBlocks]float64{}
	a.shortPos = 0
	a.shortCount = 0
	a.shortSum = 0
	a.shortMax = math.Inf(-1)
	return nil
}

// Add consumes one chunk of K-weighted interleaved samples.
func (a *Analyzer) Add(samples []float64, frames int) error {
	if frames < 0 || len(samples) < frames*a.channels {
		return fmt.Errorf("weighted sample buffer too small")
	}
	for i := 0; i < frames; i++ {
		base := i * a.channels
		for ch := 0; ch < a.channels; ch++ {
			v := samples[base+ch]
			a.segSums[ch] += v * v
		}
		a.samples++
		a.segFrames++
		if a.segFrames == a.segLen {
			a.commitSegment()
		}
	}
	return nil
}

func (a *Analyzer) commitSegment() {
	energy := 0.0
	for ch := 0; ch < a.channels; ch++ {
		energy += a.gains[ch] * a.segSums[ch] / float64(a.segLen)
		a.segSums[ch] = 0
	}
	a.segFrames = 0

	a.last4[a.last4Pos] = energy
	a.last4Pos = (a.last4Pos + 1) % momentaryBlocks
	if a.segments+1 >= momentaryBlocks {
		sum := 0.0
		for _, e := range a.last4 {
			sum += e
		}
		block := sum / momentaryBlocks
		a.blocks = append(a.blocks, block)
		if l := loudnessFromEnergy(block); l > a.momentaryMax {
			a.momentaryMax = l
		}
	}

	if a.shortCount < shortTermBlocks {
		a.shortRing[a.shortPos] = energy
		a.shortPos = (a.shortPos + 1) % shortTermBlocks
		a.shortCount++
		a.shortSum += energy
	} else {
		a.shortSum += energy - a.shortRing[a.shortPos]
		a.shortRing[a.shortPos] = energy
		a.shortPos = (a.shortPos + 1) % shortTermBlocks
	}
	if a.shortCount == shortTermBlocks {
		if l := loudnessFromEnergy(a.shortSum / shortTermBlocks); l > a.shortMax {
			a.shortMax = l
		}
	}
	a.segments++
}

// Finish applies the gates and returns the final metrics. True peak and NaN
// counts are filled by the higher-level pipeline.
func (a *Analyzer) Finish() Metrics {
	m := Metrics{
		IntegratedLUFS:   math.Inf(-1),
		ShortTermMaxLUFS: a.shortMax,
		MomentaryMaxLUFS: a.momentaryMax,
		DurationSeconds:  float64(a.samples) / float64(a.fs),
	}
	if float64(a.samples)/float64(a.fs) < 0.4 {
		m.Reason = ReasonTooShort
		return m
	}
	m.IntegratedLUFS, m.GatedBlocks, m.Reason = gateBlocks(a.blocks)
	return m
}

func loudnessFromEnergy(energy float64) float64 {
	if energy <= 0 {
		return math.Inf(-1)
	}
	return -0.691 + 10*math.Log10(energy)
}
