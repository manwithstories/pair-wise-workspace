// Package filter implements the BS.1770 K-weighting filter chain:
// a high-shelf "pre-filter" followed by a high-pass "RLB" filter.
// Coefficients are computed for the actual sample rate (De Man / ITU
// formulas), and filter state persists across calls so a stream can be
// processed frame by frame without discontinuities.
package filter

import (
	"fmt"
	"math"
)

// Biquad is a second-order IIR section in direct form I.
type Biquad struct {
	b0, b1, b2 float64
	a1, a2     float64
	x1, x2     float64 // input history
	y1, y2     float64 // output history
}

// Process filters one sample, advancing the internal state.
func (b *Biquad) Process(x float64) float64 {
	y := b.b0*x + b.b1*b.x1 + b.b2*b.x2 - b.a1*b.y1 - b.a2*b.y2
	b.x2, b.x1 = b.x1, x
	b.y2, b.y1 = b.y1, y
	return y
}

// Reset clears the filter state.
func (b *Biquad) Reset() { b.x1, b.x2, b.y1, b.y2 = 0, 0, 0, 0 }

// KWeight is the two-stage K-weighting filter for one channel.
type KWeight struct {
	shelf Biquad
	hp    Biquad
}

// MinSampleRate and MaxSampleRate bound the rates the filter supports.
const (
	MinSampleRate = 8000
	MaxSampleRate = 384000
)

// NewKWeight builds the K-weighting chain for sampleRate.
func NewKWeight(sampleRate int) (*KWeight, error) {
	if sampleRate < MinSampleRate || sampleRate > MaxSampleRate {
		return nil, fmt.Errorf("filter: unsupported sample rate %d (want %d..%d)",
			sampleRate, MinSampleRate, MaxSampleRate)
	}
	k := &KWeight{}
	k.shelf = highShelf(sampleRate)
	k.hp = highPass(sampleRate)
	return k, nil
}

// Process filters one sample through shelf then high-pass.
func (k *KWeight) Process(x float64) float64 {
	return k.hp.Process(k.shelf.Process(x))
}

// Reset clears both stages' state.
func (k *KWeight) Reset() {
	k.shelf.Reset()
	k.hp.Reset()
}

// highShelf computes the stage-1 pre-filter coefficients.
// At 48000 Hz this yields b = [1.5351249, -2.6916962, 1.1983928],
// a = [1, -1.6906593, 0.7324808].
func highShelf(sampleRate int) Biquad {
	const (
		f0 = 1681.974450955533
		g  = 3.999843853973347
		q  = 0.7071752369554196
	)
	k := math.Tan(math.Pi * f0 / float64(sampleRate))
	vh := math.Pow(10, g/20)
	vb := math.Pow(vh, 0.4996667741545416)
	a0 := 1 + k/q + k*k
	return Biquad{
		b0: (vh + vb*k/q + k*k) / a0,
		b1: 2 * (k*k - vh) / a0,
		b2: (vh - vb*k/q + k*k) / a0,
		a1: 2 * (k*k - 1) / a0,
		a2: (1 - k/q + k*k) / a0,
	}
}

// highPass computes the stage-2 RLB high-pass coefficients.
// At 48000 Hz this yields b = [1, -2, 1],
// a = [1, -1.9900475, 0.9900723].
func highPass(sampleRate int) Biquad {
	const (
		f0 = 38.13547087602444
		q  = 0.5003270373238773
	)
	k := math.Tan(math.Pi * f0 / float64(sampleRate))
	a0 := 1 + k/q + k*k
	return Biquad{
		b0: 1,
		b1: -2,
		b2: 1,
		a1: 2 * (k*k - 1) / a0,
		a2: (1 - k/q + k*k) / a0,
	}
}
