// Package gate implements BS.1770 loudness gating: 400 ms momentary
// blocks (75% overlap via 100 ms hops), a configurable short-term
// window (default 3 s), the -70 LUFS absolute gate and the -10 LU
// relative gate. Memory is bounded: block energies go into a fixed
// histogram and short-term state is a ring of per-hop sums.
package gate

import "math"

// Loudness constants from BS.1770.
const (
	OffsetLU            = -0.691
	AbsoluteGateLU      = -70.0
	RelativeGateOffset  = -10.0
	HopMs               = 100
	BlockHops           = 4 // 400 ms momentary block = 4 hops
)

// Histogram geometry: 0.1 LU bins from -100 LUFS upward.
const (
	histMinLU    = -100.0
	histBinWidth = 0.1
	histBins     = 1300 // covers up to +30 LUFS
)

// Loudness converts a weighted mean-square energy to LUFS.
func Loudness(energy float64) float64 {
	return OffsetLU + 10*math.Log10(energy)
}

// Accumulator consumes K-weighted frames channel by channel and
// maintains gating statistics. Not safe for concurrent use.
type Accumulator struct {
	weights   []float64
	hopFrames int
	stHops    int

	hopSum   float64
	hopFill  int
	hopsDone int64

	ringBlock []float64
	bp        int
	sumBlock  float64

	ringST []float64
	sp     int
	sumST  float64

	histEnergy []float64
	histCount  []int64
	blocks     int64

	stMax     float64
	stWindows int64
}

// NewAccumulator builds an accumulator for the given channel weights,
// sample rate and short-term window length (rounded to 100 ms hops,
// at least BlockHops+1 hops).
func NewAccumulator(weights []float64, sampleRate int, shortHops int) *Accumulator {
	if shortHops <= BlockHops {
		shortHops = BlockHops + 1
	}
	return &Accumulator{
		weights:    weights,
		hopFrames:  sampleRate * HopMs / 1000,
		stHops:     shortHops,
		ringBlock:  make([]float64, BlockHops),
		ringST:     make([]float64, shortHops),
		histEnergy: make([]float64, histBins),
		histCount:  make([]int64, histBins),
	}
}

// AddFrame feeds one interleaved frame (one sample per channel).
func (a *Accumulator) AddFrame(frame []float64) {
	var w float64
	for c, x := range frame {
		w += a.weights[c] * x * x
	}
	a.hopSum += w
	a.hopFill++
	if a.hopFill == a.hopFrames {
		a.pushHop(a.hopSum)
		a.hopSum = 0
		a.hopFill = 0
	}
}

// pushHop records one completed 100 ms hop of weighted square sums.
func (a *Accumulator) pushHop(s float64) {
	a.hopsDone++

	old := a.ringBlock[a.bp]
	a.ringBlock[a.bp] = s
	a.bp = (a.bp + 1) % BlockHops
	a.sumBlock += s - old
	if a.hopsDone >= BlockHops {
		e := a.sumBlock / float64(BlockHops*a.hopFrames)
		a.blocks++
		a.histAdd(e)
	}

	old = a.ringST[a.sp]
	a.ringST[a.sp] = s
	a.sp = (a.sp + 1) % a.stHops
	a.sumST += s - old
	if a.hopsDone >= int64(a.stHops) {
		e := a.sumST / float64(a.stHops*a.hopFrames)
		if e > a.stMax {
			a.stMax = e
		}
		a.stWindows++
	}
}

func binOf(lufs float64) int {
	b := int((lufs - histMinLU) / histBinWidth)
	if b < 0 {
		return 0
	}
	if b >= histBins {
		return histBins - 1
	}
	return b
}

func binLoudness(b int) float64 { return histMinLU + float64(b)*histBinWidth }

func (a *Accumulator) histAdd(energy float64) {
	if energy <= 0 {
		return // silence never passes the absolute gate; skip
	}
	b := binOf(Loudness(energy))
	a.histEnergy[b] += energy
	a.histCount[b]++
}

// Blocks returns the number of complete 400 ms blocks seen.
func (a *Accumulator) Blocks() int64 { return a.blocks }

// Integrated computes the gated integrated loudness. ok is false when
// every block fell below the absolute gate (e.g. silence) — callers
// must report "undecidable" rather than a fabricated number.
func (a *Accumulator) Integrated() (lufs float64, gated int64, ok bool) {
	var sum1 float64
	var count1 int64
	for b := 0; b < histBins; b++ {
		if binLoudness(b) > AbsoluteGateLU {
			sum1 += a.histEnergy[b]
			count1 += a.histCount[b]
		}
	}
	if count1 == 0 {
		return 0, 0, false
	}
	relThresh := Loudness(sum1/float64(count1)) + RelativeGateOffset
	if relThresh < AbsoluteGateLU {
		relThresh = AbsoluteGateLU
	}
	var sum2 float64
	var count2 int64
	for b := 0; b < histBins; b++ {
		if binLoudness(b) > relThresh {
			sum2 += a.histEnergy[b]
			count2 += a.histCount[b]
		}
	}
	if count2 == 0 {
		return 0, 0, false
	}
	return Loudness(sum2 / float64(count2)), count2, true
}

// ShortTermMax returns the maximum short-term loudness over all full
// short-term windows. ok is false when no full window was seen or all
// windows were silent.
func (a *Accumulator) ShortTermMax() (lufs float64, ok bool) {
	if a.stWindows == 0 || a.stMax <= 0 {
		return 0, false
	}
	return Loudness(a.stMax), true
}
