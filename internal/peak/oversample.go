// Package peak estimates true peak via polyphase oversampling (4x or 8x)
// with a windowed-sinc interpolation filter, per BS.1770. State is
// kept per channel so streams can be processed incrementally.
package peak

import (
	"fmt"
	"math"
)

// tapsPerPhase is the FIR length per polyphase branch.
const tapsPerPhase = 16

// TruePeak tracks the running true peak of one channel.
type TruePeak struct {
	factor int
	phases [][]float64 // polyphase branches, gain-compensated
	hist   []float64   // input sample history, ring buffer
	pos    int
	max    float64
}

// NewTruePeak builds an oversampler with the given factor (4 or 8).
func NewTruePeak(factor int) (*TruePeak, error) {
	if factor != 4 && factor != 8 {
		return nil, fmt.Errorf("peak: unsupported oversampling factor %d (want 4 or 8)", factor)
	}
	taps := designLowpass(factor)
	phases := make([][]float64, factor)
	for p := 0; p < factor; p++ {
		ph := make([]float64, tapsPerPhase)
		for t := 0; t < tapsPerPhase; t++ {
			ph[t] = taps[p+t*factor] * float64(factor) // unity passband gain
		}
		phases[p] = ph
	}
	return &TruePeak{
		factor: factor,
		phases: phases,
		hist:   make([]float64, tapsPerPhase),
	}, nil
}

// designLowpass returns a Blackman-windowed sinc lowpass of
// factor*tapsPerPhase taps with cutoff just below the input Nyquist
// (normalized to the output rate).
func designLowpass(factor int) []float64 {
	n := factor * tapsPerPhase
	cutoff := 0.5 / float64(factor) * 0.95
	h := make([]float64, n)
	m := float64(n-1) / 2
	var sum float64
	for i := 0; i < n; i++ {
		x := float64(i) - m
		var s float64
		if x == 0 {
			s = 2 * cutoff
		} else {
			s = math.Sin(2*math.Pi*cutoff*x) / (math.Pi * x)
		}
		w := 0.42 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1)) +
			0.08*math.Cos(4*math.Pi*float64(i)/float64(n-1))
		h[i] = s * w
		sum += h[i]
	}
	for i := range h {
		h[i] /= sum
	}
	return h
}

// Process feeds one input sample and updates the running peak with
// both the sample itself and the interpolated values.
func (t *TruePeak) Process(x float64) {
	if ax := math.Abs(x); ax > t.max {
		t.max = ax
	}
	t.hist[t.pos] = x
	t.pos = (t.pos + 1) % tapsPerPhase
	for p := 0; p < t.factor; p++ {
		ph := t.phases[p]
		var acc float64
		idx := t.pos
		for k := 0; k < tapsPerPhase; k++ {
			acc += ph[k] * t.hist[idx]
			idx++
			if idx == tapsPerPhase {
				idx = 0
			}
		}
		if a := math.Abs(acc); a > t.max {
			t.max = a
		}
	}
}

// Finish flushes the filter tail with zeros so transients near the end
// of the stream are fully interpolated.
func (t *TruePeak) Finish() {
	for i := 0; i < tapsPerPhase; i++ {
		t.Process(0)
	}
}

// Max returns the running true peak as a linear amplitude.
func (t *TruePeak) Max() float64 { return t.max }

// DBTP converts a linear peak to dBTP. ok is false for a zero peak
// (digital silence), where no finite dB value exists.
func DBTP(linear float64) (db float64, ok bool) {
	if linear <= 0 {
		return 0, false
	}
	return 20 * math.Log10(linear), true
}
