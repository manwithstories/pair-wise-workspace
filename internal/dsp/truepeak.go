package dsp

import "math"

const (
	OversampleFactor = 4
	tapsPerPhase     = 16
)

// Each branch is a delayed version of the normalized low-pass prototype.
// Branch k implements fractional delay k/4 of an input sample so the four
// branches together yield 4x oversampled values with unit DC gain.
func designPolyphase() [][]float64 {
	phases := make([][]float64, OversampleFactor)
	cutoff := 0.45
	for branch := 0; branch < OversampleFactor; branch++ {
		coef := make([]float64, tapsPerPhase)
		var sum float64
		for tap := 0; tap < tapsPerPhase; tap++ {
			center := float64(tap) - float64(tapsPerPhase-1)/2.0
			delay := center - float64(branch)/float64(OversampleFactor)
			window := 0.5 * (1 - math.Cos(2*math.Pi*float64(tap)/float64(tapsPerPhase-1)))
			sinc := 2 * cutoff
			if delay != 0 {
				sinc = math.Sin(2*math.Pi*cutoff*delay) / (math.Pi * delay)
			}
			coef[tap] = window * sinc
			sum += coef[tap]
		}
		for tap := range coef {
			coef[tap] /= sum
		}
		phases[branch] = coef
	}
	return phases
}

type peakState struct {
	history [tapsPerPhase]float64
	head    int
	filled  int
	peak    float64
}

type TruePeakMeter struct {
	phases [][]float64
	states []peakState
}

func NewTruePeakMeter(channels int) *TruePeakMeter {
	return &TruePeakMeter{
		phases: designPolyphase(),
		states: make([]peakState, channels),
	}
}

func (m *TruePeakMeter) Reset() {
	for i := range m.states {
		m.states[i] = peakState{}
	}
}

func (m *TruePeakMeter) Process(planar [][]float64) {
	for ch, samples := range planar {
		if ch >= len(m.states) {
			continue
		}
		state := &m.states[ch]
		for _, sample := range samples {
			state.history[state.head] = sample
			state.head = (state.head + 1) % tapsPerPhase
			if state.filled < tapsPerPhase {
				state.filled++
			}
			for phase := 0; phase < OversampleFactor; phase++ {
				coef := m.phases[phase]
				var value float64
				index := state.head - 1
				for tap := 0; tap < state.filled; tap++ {
					if index < 0 {
						index += tapsPerPhase
					}
					value += coef[tap] * state.history[index]
					index--
				}
				if abs := math.Abs(value); abs > state.peak {
					state.peak = abs
				}
			}
		}
	}
}

func (m *TruePeakMeter) TruePeakLinear() float64 {
	var peak float64
	for i := range m.states {
		if m.states[i].peak > peak {
			peak = m.states[i].peak
		}
	}
	return peak
}

func (m *TruePeakMeter) TruePeakDBTP() float64 {
	peak := m.TruePeakLinear()
	if peak <= 0 || math.IsNaN(peak) {
		return math.Inf(-1)
	}
	return 20 * math.Log10(peak)
}
