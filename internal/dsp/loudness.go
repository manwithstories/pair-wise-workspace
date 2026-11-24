package dsp

import (
	"math"

	"mediad/internal/pcm"
)

const (
	TargetLUFS     = -16.0
	CeilingDBTP    = -1.0
	AbsoluteGate   = -70.0
	RelativeGateDB = -10.0
	BlockFrames    = 19200 // 400 ms at 48 kHz
	HopFrames      = 4800  // 100 ms at 48 kHz
	SilenceLUFS    = -120.0
)

// LoudnessReport contains deterministic BS.1770-style measurements and action.
type LoudnessReport struct {
	IntegratedLUFS    float64 `json:"integrated_lufs"`
	TruePeakDBTP      float64 `json:"true_peak_dbtp"`
	Gain              float64 `json:"gain"`
	GatedBlocks       int     `json:"gated_blocks"`
	AnalyzedBlocks    int     `json:"analyzed_blocks"`
	TargetLUFS        float64 `json:"target_lufs"`
	CeilingDBTP       float64 `json:"ceiling_dbtp"`
	UpwardGainAllowed bool    `json:"upward_gain_allowed"`
}

type biquad struct {
	b0, b1, b2 float64
	a1, a2     float64
	x1, x2     float64
	y1, y2     float64
}

func (f *biquad) process(x float64) float64 {
	y := f.b0*x + f.b1*f.x1 + f.b2*f.x2 - f.a1*f.y1 - f.a2*f.y2
	f.x2, f.x1 = f.x1, x
	f.y2, f.y1 = f.y1, y
	return y
}

// kWeight applies the two BS.1770 K-weighting stages. Coefficients are the
// standard 48 kHz coefficient set; the pipeline resamples before this point.
func kWeight(samples []float64, channels int) []float64 {
	out := make([]float64, len(samples))
	for ch := 0; ch < channels; ch++ {
		shelf := biquad{b0: 1.53512485958697, b1: -2.69169618940638, b2: 1.19839281085285, a1: -1.69065929318241, a2: 0.73248077421585}
		rlb := biquad{b0: 1.0, b1: -2.0, b2: 1.0, a1: -1.99004745483398, a2: 0.99007225036621}
		for i := ch; i < len(samples); i += channels {
			out[i] = rlb.process(shelf.process(samples[i]))
		}
	}
	return out
}

func blockMeanSquares(weighted []float64, channels, start int) float64 {
	var sum float64
	for ch := 0; ch < channels; ch++ {
		var ms float64
		n := 0
		for frame := start; frame < start+BlockFrames; frame++ {
			v := weighted[frame*channels+ch]
			ms += v * v
			n++
		}
		ms /= float64(n)
		// Stereo/mono weights are both 1.0 in BS.1770's main channel set.
		sum += ms
	}
	return sum
}

// IntegratedLoudness computes absolute- and relative-gated integrated loudness.
func IntegratedLoudness(in *pcm.Audio) (float64, int, int) {
	if in == nil || in.Frames < BlockFrames {
		// Short generated unit tests may still be measured over their full
		// extent via measureShort; production demo is 12 seconds.
		return measureShortLoudness(in), 0, 0
	}
	weighted := kWeight(in.Samples, in.Channels)
	type block struct{ ms float64 }
	var blocks []block
	for start := 0; start+BlockFrames <= in.Frames; start += HopFrames {
		ms := blockMeanSquares(weighted, in.Channels, start)
		lufs := -0.691 + 10*math.Log10(math.Max(ms, 1e-20))
		if lufs > AbsoluteGate {
			blocks = append(blocks, block{ms: ms})
		}
	}
	if len(blocks) == 0 {
		return SilenceLUFS, 0, 0
	}
	var sum float64
	for _, b := range blocks {
		sum += b.ms
	}
	ungated := -0.691 + 10*math.Log10(sum/float64(len(blocks)))
	relativeThreshold := ungated + RelativeGateDB

	var gatedSum float64
	gated := 0
	for _, b := range blocks {
		lufs := -0.691 + 10*math.Log10(b.ms)
		if lufs > relativeThreshold && lufs > AbsoluteGate {
			gatedSum += b.ms
			gated++
		}
	}
	if gated == 0 {
		return SilenceLUFS, len(blocks), 0
	}
	return -0.691 + 10*math.Log10(gatedSum/float64(gated)), len(blocks), gated
}

func measureShortLoudness(in *pcm.Audio) float64 {
	if in == nil || len(in.Samples) == 0 {
		return SilenceLUFS
	}
	weighted := kWeight(in.Samples, in.Channels)
	var sum float64
	for _, v := range weighted {
		sum += v * v
	}
	ms := sum / float64(len(weighted)) * float64(in.Channels)
	if ms <= 1e-20 {
		return SilenceLUFS
	}
	return -0.691 + 10*math.Log10(ms)
}

// TruePeak estimates inter-sample peaks with deterministic 4x cubic oversampling.
func TruePeak(in *pcm.Audio) float64 {
	if in == nil || in.Frames == 0 {
		return 0
	}
	peak := 0.0
	for ch := 0; ch < in.Channels; ch++ {
		at := func(frame int) float64 {
			if frame < 0 {
				frame = 0
			}
			if frame >= in.Frames {
				frame = in.Frames - 1
			}
			return in.Samples[frame*in.Channels+ch]
		}
		for i := 0; i < in.Frames; i++ {
			y0, y1, y2, y3 := at(i-1), at(i), at(i+1), at(i+2)
			peak = math.Max(peak, math.Abs(y1))
			// Cubic Hermite midpoint/quarter estimates between y1 and y2.
			for _, t := range []float64{0.25, 0.5, 0.75} {
				v := 0.5 * ((2 * y1) + (-y0+y2)*t +
					(2*y0-5*y1+4*y2-y3)*t*t +
					(-y0+3*y1-3*y2+y3)*t*t*t)
				peak = math.Max(peak, math.Abs(v))
			}
		}
	}
	return peak
}

// ProcessLoudness applies peak-limited gain. It deliberately never applies
// positive gain: missing-packet silence and low-level ambience therefore cannot
// be upward-expanded into audible noise.
func ProcessLoudness(in *pcm.Audio) (*pcm.Audio, LoudnessReport, error) {
	lufs, analyzed, gated := IntegratedLoudness(in)
	tp := TruePeak(in)
	tpDB := SilenceLUFS
	if tp > 0 {
		tpDB = 20 * math.Log10(tp)
	}
	ceiling := math.Pow(10, CeilingDBTP/20)

	gain := 1.0
	if lufs > SilenceLUFS+1 {
		loudnessGain := math.Pow(10, (TargetLUFS-lufs)/20)
		gain = math.Min(gain, loudnessGain)
	}
	if tp > 0 {
		gain = math.Min(gain, ceiling/tp)
	}
	// A tiny numerical safety margin guarantees the post-gain estimate remains
	// below the declared ceiling.
	gain *= 0.999999
	if gain > 1 {
		gain = 1
	}

	out := in.Clone()
	for i := range out.Samples {
		out.Samples[i] *= gain
	}
	if postTP := TruePeak(out); postTP > ceiling {
		safety := ceiling / postTP
		for i := range out.Samples {
			out.Samples[i] *= safety
		}
		gain *= safety
	}

	report := LoudnessReport{
		IntegratedLUFS: lufs, TruePeakDBTP: tpDB, Gain: gain,
		GatedBlocks: gated, AnalyzedBlocks: analyzed,
		TargetLUFS: TargetLUFS, CeilingDBTP: CeilingDBTP, UpwardGainAllowed: false,
	}
	return out, report, nil
}
