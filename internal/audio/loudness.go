package audio

import (
	"math"

	"mediapipeline/internal/mediaerr"
)

// silenceDB is the stable dBFS value reported for zero-energy windows,
// avoiding math.Log(0) = -Inf floating-point noise in quantified reports.
const silenceDB = -200.0

// WindowStats is the quantified loudness result of one analysis window.
type WindowStats struct {
	StartSample int64
	Length      int
	RMS         float64 // linear root-mean-square amplitude
	Peak        float64 // maximum absolute sample
	LoudnessDB  float64 // 20*log10(RMS), clamped at silenceDB for silence
}

// LoudnessMeter measures RMS/peak loudness over fixed-size windows while
// carrying partial-window samples across input blocks. A sub-window tail at
// flush time is measured with the same formula so every input gets a stable
// result, including empty and all-silence streams.
type LoudnessMeter struct {
	windowSize int
	carry      []float64
	carryStart int64
	totalIn    int64
	windows    int64
}

// NewLoudnessMeter validates the window size and builds a meter.
func NewLoudnessMeter(windowSize int) (*LoudnessMeter, *mediaerr.Error) {
	if windowSize <= 0 {
		return nil, mediaerr.New(mediaerr.StageLoudness, mediaerr.KindInvalidInput,
			"loudness window must contain at least one sample")
	}
	return &LoudnessMeter{windowSize: windowSize}, nil
}

// Process consumes a block and returns stats for every complete window it
// completed, carrying the remainder. Empty blocks are a defined no-op.
func (m *LoudnessMeter) Process(block []float64) ([]WindowStats, *mediaerr.Error) {
	if len(block) == 0 {
		return nil, nil
	}
	m.carry = append(m.carry, block...)
	var out []WindowStats
	for len(m.carry) >= m.windowSize {
		out = append(out, m.measure(m.carry[:m.windowSize], m.carryStart))
		m.carryStart += int64(m.windowSize)
		m.carry = append([]float64(nil), m.carry[m.windowSize:]...)
	}
	m.totalIn += int64(len(block))
	return out, nil
}

// Flush measures the remaining sub-window tail (if any) and resets the meter.
// The tail is measured over its actual length, so a shorter final window is
// never padded with fake silence. With no samples at all it returns silence
// stats with zero length rather than an error.
func (m *LoudnessMeter) Flush() WindowStats {
	var stats WindowStats
	if len(m.carry) > 0 {
		stats = m.measure(m.carry, m.carryStart)
	} else {
		stats = WindowStats{LoudnessDB: silenceDB}
	}
	m.carry, m.carryStart, m.totalIn, m.windows = nil, 0, 0, 0
	return stats
}

// measure computes RMS, peak and dBFS loudness over one slice.
func (m *LoudnessMeter) measure(samples []float64, start int64) WindowStats {
	var sumSq, peak float64
	for _, v := range samples {
		a := math.Abs(v)
		if a > peak {
			peak = a
		}
		sumSq += v * v
	}
	rms := math.Sqrt(sumSq / float64(len(samples)))
	db := silenceDB
	if rms > 0 {
		db = 20 * math.Log10(rms)
	}
	m.windows++
	return WindowStats{
		StartSample: start,
		Length:      len(samples),
		RMS:         rms,
		Peak:        peak,
		LoudnessDB:  db,
	}
}

// WindowsMeasured returns the number of completed windows (excluding flush).
func (m *LoudnessMeter) WindowsMeasured() int64 { return m.windows }

// Limiter applies a brick-wall peak ceiling. Each block is scaled by a single
// uniform gain min(1, ceiling/peak), which caps peaks without the harmonic
// distortion of hard clipping. Silence and sub-ceiling blocks pass through
// unchanged and the limiter holds no cross-block state.
type Limiter struct {
	ceiling float64
}

// NewLimiter validates and builds a peak limiter; ceiling must be in (0, 1].
func NewLimiter(ceiling float64) (*Limiter, *mediaerr.Error) {
	if ceiling <= 0 || ceiling > 1 {
		return nil, mediaerr.New(mediaerr.StageLoudness, mediaerr.KindInvalidInput,
			"limiter ceiling must be within (0, 1]")
	}
	return &Limiter{ceiling: ceiling}, nil
}

// Limit returns a scaled copy of block whose peak never exceeds the ceiling.
// An empty block yields an empty slice. The applied gain is also returned for
// inclusion in the quantified report.
func (l *Limiter) Limit(block []float64) (out []float64, gain float64) {
	gain = 1.0
	if len(block) == 0 {
		return []float64{}, gain
	}
	var peak float64
	for _, v := range block {
		if a := math.Abs(v); a > peak {
			peak = a
		}
	}
	if peak > l.ceiling {
		gain = l.ceiling / peak
	}
	out = make([]float64, len(block))
	for i, v := range block {
		out[i] = v * gain
	}
	return out, gain
}

// Ceiling returns the configured peak ceiling.
func (l *Limiter) Ceiling() float64 { return l.ceiling }
