package loudness

import (
	"fmt"
	"math"
	"sync"
)

const (
	truePeakLookahead = 12
	truePeakPhases    = 4 // raw sample plus three interpolated phases
	truePeakSize      = 2 * truePeakLookahead
)

var (
	tpOnce   sync.Once
	tpCoeffs [truePeakPhases - 1][truePeakSize]float64
)

func initTruePeakCoefficients() {
	for phase := 1; phase < truePeakPhases; phase++ {
		d := float64(phase) / truePeakPhases
		sum := 0.0
		for i := 0; i < truePeakSize; i++ {
			offset := float64(i-(truePeakLookahead-1)) - d
			window := lanczos(offset, truePeakLookahead)
			tpCoeffs[phase-1][i] = sinc(offset) * window
			sum += tpCoeffs[phase-1][i]
		}
		if sum != 0 {
			for i := range tpCoeffs[phase-1] {
				tpCoeffs[phase-1][i] /= sum
			}
		}
	}
}

// TruePeak runs a 4x polyphase interpolator. Interpolated points are delayed
// by the lookahead; raw sample peaks are always included so file boundaries
// are not lost.
type TruePeak struct {
	channels int
	hist     [][]float64
	pos      []int
	count    int
	peak     float64
}

func NewTruePeak(channels int) (*TruePeak, error) {
	t := &TruePeak{}
	if err := t.Reset(channels); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *TruePeak) Reset(channels int) error {
	if channels <= 0 {
		return fmt.Errorf("true-peak channel count must be positive")
	}
	tpOnce.Do(initTruePeakCoefficients)
	t.channels = channels
	if cap(t.hist) < channels {
		t.hist = make([][]float64, channels)
		t.pos = make([]int, channels)
	} else {
		t.hist = t.hist[:channels]
		t.pos = t.pos[:channels]
	}
	for ch := range t.hist {
		if cap(t.hist[ch]) < truePeakSize {
			t.hist[ch] = make([]float64, truePeakSize)
		} else {
			t.hist[ch] = t.hist[ch][:truePeakSize]
			for i := range t.hist[ch] {
				t.hist[ch][i] = 0
			}
		}
		t.pos[ch] = 0
	}
	t.count = 0
	t.peak = 0
	return nil
}

// Add scans one interleaved chunk. The phase filters run only once a full
// interpolation window is available.
func (t *TruePeak) Add(samples []float64, frames int) error {
	if frames < 0 || len(samples) < frames*t.channels {
		return fmt.Errorf("true-peak sample buffer too small")
	}
	for i := 0; i < frames; i++ {
		base := i * t.channels
		for ch := 0; ch < t.channels; ch++ {
			x := samples[base+ch]
			if a := math.Abs(x); a > t.peak {
				t.peak = a
			}
			t.hist[ch][t.pos[ch]] = x
			t.pos[ch] = (t.pos[ch] + 1) % truePeakSize
		}
		t.count++
		if t.count < truePeakSize {
			continue
		}
		for ch := 0; ch < t.channels; ch++ {
			for phase := range tpCoeffs {
				idx := t.pos[ch]
				y := 0.0
				for _, c := range tpCoeffs[phase] {
					y += c * t.hist[ch][idx]
					idx++
					if idx == truePeakSize {
						idx = 0
					}
				}
				if a := math.Abs(y); a > t.peak {
					t.peak = a
				}
			}
		}
	}
	return nil
}

// DBTP converts the largest sample/interpolated absolute value to dBTP.
func (t *TruePeak) DBTP() float64 {
	if t.peak <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(t.peak)
}

func (t *TruePeak) Peak() float64 { return t.peak }

func sinc(x float64) float64 {
	if math.Abs(x) < 1e-12 {
		return 1
	}
	return math.Sin(math.Pi*x) / (math.Pi * x)
}

func lanczos(x float64, a int) float64 {
	if x <= -float64(a) || x >= float64(a) {
		return 0
	}
	return sinc(x / float64(a))
}
