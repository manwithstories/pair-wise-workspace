package av

import (
	"math"

	"mediad/internal/media"
)

// Delivery loudness targets.
const (
	TargetLUFS  = -16.0
	MaxTruePeak = -1.0 // dBTP
	LoudnessTol = 1.0  // integrated LUFS tolerance
)

// LoudnessReport summarises one rendered delivery.
type LoudnessReport struct {
	IntegratedLUFS float64 `json:"integrated_lufs"`
	TruePeakDBTP   float64 `json:"true_peak_dbtp"`
	SamplePeakDBFS float64 `json:"sample_peak_dbfs"`
	ClippedSamples int64   `json:"clipped_samples"`
	SilentSeconds  float64 `json:"silent_seconds"`
	SilentRuns     int     `json:"silent_runs"`
	GainDB         float64 `json:"gain_db"`
	Frames         int64   `json:"frames"`
}

// MeasureInterleaved analyses an interleaved stereo float bus at 48 kHz.
// Loudness follows BS.1770-4: K-weighting, 400 ms gated blocks with a -70 LUFS
// absolute gate and a -10 LU relative gate. True peak is estimated with 4x
// oversampling; clipping counts samples at or beyond full scale.
func MeasureInterleaved(samples []float32) LoudnessReport {
	var rep LoudnessReport
	frames := len(samples) / 2
	rep.Frames = int64(frames)
	if frames == 0 {
		rep.IntegratedLUFS = -70
		rep.TruePeakDBTP = -120
		rep.SamplePeakDBFS = -120
		return rep
	}

	// K-weighting via two biquads (high shelf + high pass) at 48 kHz.
	shelf := (&biquad{}).highShelf(OutputRate, 1681.974450955533, 0.7071752, 3.999843853973347)
	highpass := (&biquad{}).highPass(OutputRate, 38.13547087602444, 0.5003270373238773)

	left := make([]float64, frames)
	right := make([]float64, frames)
	var samplePeak float64
	clipCount := int64(0)
	for i := 0; i < frames; i++ {
		l := float64(samples[i*2])
		r := float64(samples[i*2+1])
		al, ar := math.Abs(l), math.Abs(r)
		if al > samplePeak {
			samplePeak = al
		}
		if ar > samplePeak {
			samplePeak = ar
		}
		if al >= 1.0 || ar >= 1.0 {
			clipCount++
		}
		left[i] = shelf.process(highpass.process(l))
		right[i] = shelf.processR(highpass.processR(r))
	}
	rep.ClippedSamples = clipCount
	rep.SamplePeakDBFS = dbfs(samplePeak)

	// 400 ms blocks with 100 ms hop, BS.1770 channel weights 1.0 for L/R.
	const blockLen = OutputRate * 4 / 10
	const hopLen = OutputRate / 10
	type block struct{ power float64 }
	var blocks []block
	for start := 0; start+blockLen <= frames; start += hopLen {
		var sum float64
		for i := start; i < start+blockLen; i++ {
			sum += left[i]*left[i] + right[i]*right[i]
		}
		blocks = append(blocks, block{power: sum / float64(blockLen)})
	}
	if len(blocks) == 0 {
		// Short signals: use one block over whatever exists.
		var sum float64
		for i := 0; i < frames; i++ {
			sum += left[i]*left[i] + right[i]*right[i]
		}
		blocks = append(blocks, block{power: sum / float64(frames)})
	}
	blockLoudness := func(p float64) float64 {
		if p <= 0 {
			return -70
		}
		return -0.691 + 10*math.Log10(p)
	}
	// Absolute gate.
	var gated []float64
	for _, b := range blocks {
		if l := blockLoudness(b.power); l >= -70 {
			gated = append(gated, b.power)
		}
	}
	if len(gated) == 0 {
		rep.IntegratedLUFS = -70
	} else {
		// Relative gate at mean(gated) - 10 LU.
		var mean float64
		for _, p := range gated {
			mean += p
		}
		mean /= float64(len(gated))
		relGate := blockLoudness(mean) - 10
		var sum, n float64
		for _, p := range gated {
			if blockLoudness(p) >= relGate {
				sum += p
				n++
			}
		}
		if n == 0 {
			rep.IntegratedLUFS = -70
		} else {
			rep.IntegratedLUFS = blockLoudness(sum / n)
		}
	}

	// True peak: 4x oversample with a windowed sinc; track the maximum.
	rep.TruePeakDBTP = dbtp(oversampledPeak(samples))

	// Silence detection: contiguous runs of stereo frames under -60 dBFS for
	// at least 100 ms.
	const silenceThresh = 0.001
	const minRun = OutputRate / 10
	run := 0
	for i := 0; i < frames; i++ {
		amp := math.Max(math.Abs(float64(samples[i*2])), math.Abs(float64(samples[i*2+1])))
		if amp < silenceThresh {
			run++
			if run >= minRun {
				// Count this whole run once at its end; mark by tracking start.
			}
		} else {
			if run >= minRun {
				rep.SilentRuns++
				rep.SilentSeconds += float64(run) / OutputRate
			}
			run = 0
		}
	}
	if run >= minRun {
		rep.SilentRuns++
		rep.SilentSeconds += float64(run) / OutputRate
	}
	return rep
}

// NormalizeGain computes a gain (linear) that brings an integrated loudness
// measurement to the target, clamped so true peak stays at or below -1 dBTP.
// Silence-only material is never amplified.
func NormalizeGain(rep LoudnessReport) float64 {
	if rep.Frames == 0 {
		return 1
	}
	// Never amplify an effectively silent programme.
	if rep.IntegratedLUFS <= -69 || rep.SamplePeakDBFS <= -70 {
		return 1
	}
	gainDB := TargetLUFS - rep.IntegratedLUFS
	maxGain := MaxTruePeak - rep.TruePeakDBTP
	if gainDB > maxGain {
		gainDB = maxGain
	}
	gain := math.Pow(10, gainDB/20)
	if math.IsNaN(gain) || math.IsInf(gain, 0) || gain <= 0 {
		return 1
	}
	return gain
}

// ApplyGain scales interleaved samples in place, reporting the achieved peak.
func ApplyGain(samples []float32, gain float64) {
	for i := range samples {
		samples[i] = float32(float64(samples[i]) * gain)
	}
}

func dbfs(v float64) float64 {
	if v <= 0 {
		return -144
	}
	return 20 * math.Log10(v)
}

func dbtp(v float64) float64 {
	if v <= 0 {
		return -144
	}
	return 20 * math.Log10(v)
}

// oversampledPeak estimates inter-sample peak with a 4x Hann-windowed sinc.
func oversampledPeak(samples []float32) float64 {
	frames := len(samples) / 2
	if frames == 0 {
		return 0
	}
	const taps = 16
	var peak float64
	for ch := 0; ch < 2; ch++ {
		for n := 0; n < frames; n++ {
			a := math.Abs(float64(samples[n*2+ch]))
			if a > peak {
				peak = a
			}
			for k := 1; k < 4; k++ {
				t := float64(k) / 4
				var acc float64
				for j := -taps + 1; j <= taps; j++ {
					idx := n + j
					if idx < 0 || idx >= frames {
						continue
					}
					x := float64(samples[idx*2+ch])
					// Sinc kernel with Hann window.
					d := t - float64(j)
					var sinc float64
					if math.Abs(d) < 1e-9 {
						sinc = 1
					} else {
						sinc = math.Sin(math.Pi*d) / (math.Pi * d)
					}
					w := 0.5 + 0.5*math.Cos(math.Pi*d/float64(taps))
					acc += x * sinc * w
				}
				if a := math.Abs(acc); a > peak {
					peak = a
				}
			}
		}
	}
	return peak
}

// biquad is a direct-form I second-order section.
type biquad struct {
	b0, b1, b2, a1, a2 float64
	x1, x2, y1, y2     float64
	// Second state for the right channel.
	x1r, x2r, y1r, y2r float64
}

func (b *biquad) process(x float64) float64 {
	y := b.b0*x + b.b1*b.x1 + b.b2*b.x2 - b.a1*b.y1 - b.a2*b.y2
	b.x2, b.x1 = b.x1, x
	b.y2, b.y1 = b.y1, y
	return y
}

func (b *biquad) processR(x float64) float64 {
	y := b.b0*x + b.b1*b.x1r + b.b2*b.x2r - b.a1*b.y1r - b.a2*b.y2r
	b.x2r, b.x1r = b.x1r, x
	b.y2r, b.y1r = b.y1r, y
	return y
}

func (b *biquad) highShelf(sampleRate float64, f0, q, gainDB float64) biquad {
	// RBJ audio EQ cookbook high shelf.
	a := math.Pow(10, gainDB/40)
	w0 := 2 * math.Pi * f0 / sampleRate
	alpha := math.Sin(w0) / (2 * q)
	cosw := math.Cos(w0)
	sqrtA := math.Sqrt(a)
	b0 := a * ((a + 1) + (a-1)*cosw + 2*sqrtA*alpha)
	b1 := -2 * a * ((a - 1) + (a+1)*cosw)
	b2 := a * ((a + 1) + (a-1)*cosw - 2*sqrtA*alpha)
	a0 := (a + 1) - (a-1)*cosw + 2*sqrtA*alpha
	a1 := 2 * ((a - 1) - (a+1)*cosw)
	a2 := (a + 1) - (a-1)*cosw - 2*sqrtA*alpha
	return biquad{b0: b0 / a0, b1: b1 / a0, b2: b2 / a0, a1: a1 / a0, a2: a2 / a0}
}

func (b *biquad) highPass(sampleRate float64, f0, q float64) biquad {
	w0 := 2 * math.Pi * f0 / sampleRate
	alpha := math.Sin(w0) / (2 * q)
	cosw := math.Cos(w0)
	b0 := (1 + cosw) / 2
	b1 := -(1 + cosw)
	b2 := (1 + cosw) / 2
	a0 := 1 + alpha
	a1 := -2 * cosw
	a2 := 1 - alpha
	return biquad{b0: b0 / a0, b1: b1 / a0, b2: b2 / a0, a1: a1 / a0, a2: a2 / a0}
}

// FormatLoudness renders a report as stable key=value tokens.
func FormatLoudness(rep LoudnessReport) string {
	return media.FormatKV(
		"integrated_lufs", media.FormatFloat(rep.IntegratedLUFS, 2),
		"true_peak_dbtp", media.FormatFloat(rep.TruePeakDBTP, 2),
		"clipped_samples", media.FormatInt(rep.ClippedSamples),
		"silent_seconds", media.FormatFloat(rep.SilentSeconds, 3),
	)
}
