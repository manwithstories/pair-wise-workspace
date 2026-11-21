package audio

import (
	"math"
)

// LoudnessStats summarizes one processed fragment.
type LoudnessStats struct {
	IntegratedLUFS float64 // measured before normalization
	TargetLUFS     float64
	GainDB         float64 // applied linear gain in dB
	SilenceFrames  int     // frames at or below the silence gate
	ClipFrames     int     // frames that hit full-scale after gain
	GatedBlocks    int     // 400 ms blocks retained under BS.1770 gating
}

const (
	blockFrames   = 19200 // 400 ms at 48 kHz
	silenceThresh = -70.0 // LUFS-equivalent floor treated as silence
	absoluteGate  = -70.0
	relativeGate  = -10.0
)

// Normalize measures integrated loudness and applies a single linear gain so
// the result anchors at -23 LUFS. Silence and post-gain clipping are counted,
// never hidden; a silent fragment gets no gain.
func Normalize(f Frame) (Frame, LoudnessStats) {
	if f.SampleRate != TargetRate || f.Channels != TargetChannels {
		panic("audio: Normalize requires converted 48 kHz stereo")
	}
	frames := len(f.Samples) / TargetChannels
	blocks := blockLoudness(f.Samples, frames)
	stats := LoudnessStats{TargetLUFS: TargetLUFS}
	integrated, gated := integrate(blocks)
	stats.IntegratedLUFS = integrated
	stats.GatedBlocks = gated
	gainDB := 0.0
	if !math.IsInf(integrated, 0) {
		gainDB = TargetLUFS - integrated
	}
	stats.GainDB = gainDB
	gain := math.Pow(10, gainDB/20)
	out, clipFrames := ApplyGain(f.Samples, gain)
	stats.ClipFrames = clipFrames
	// Silence is judged over 10 ms windows on mean-square energy, so tone
	// zero crossings do not count as silence.
	const win = 480
	noiseFloor := math.Pow(10, silenceThresh/10)
	for w0 := 0; w0 < frames; w0 += win {
		w1 := w0 + win
		if w1 > frames {
			w1 = frames
		}
		energy := 0.0
		for i := w0; i < w1; i++ {
			for ch := 0; ch < TargetChannels; ch++ {
				idx := i*TargetChannels + ch
				v := float64(f.Samples[idx]) / 32768.0
				energy += v * v
			}
		}
		if energy/float64((w1-w0)*TargetChannels) < noiseFloor {
			stats.SilenceFrames += w1 - w0
		}
	}
	return Frame{SampleRate: TargetRate, Channels: TargetChannels, Samples: out}, stats
}

// ApplyGain scales samples by a linear gain with hard full-scale limiting.
// It returns the clamped samples and the frame count (per stereo frame) of
// 10 ms windows containing at least one clipped sample.
func ApplyGain(samples []int16, gain float64) ([]int16, int) {
	out := make([]int16, len(samples))
	const win = 480
	clipFrames := 0
	frames := len(samples) / TargetChannels
	for w0 := 0; w0 < frames; w0 += win {
		w1 := w0 + win
		if w1 > frames {
			w1 = frames
		}
		clipped := false
		for i := w0 * TargetChannels; i < w1*TargetChannels; i++ {
			r := int32(math.Round(float64(samples[i]) * gain))
			if r > 32767 {
				r, clipped = 32767, true
			} else if r < -32768 {
				r, clipped = -32768, true
			}
			out[i] = int16(r)
		}
		if clipped {
			clipFrames += w1 - w0
		}
	}
	return out, clipFrames
}

// blockLoudness returns per-block mean-square energy after a one-pole
// approximation of the BS.1770 K-weighting stage, per channel.
func blockLoudness(samples []int16, frames int) []float64 {
	var blocks []float64
	// K-weighting high-shelf coefficients (stage 1, ~1.5 kHz shelf).
	const a1 = 0.976
	z := [2]float64{0, 0}
	energy := [2]float64{0, 0}
	n := 0
	flush := func() {
		if n == 0 {
			return
		}
		ms := (energy[0] + energy[1]) / float64(n) / 2
		blocks = append(blocks, -0.691+10*math.Log10(ms+1e-12))
		energy = [2]float64{0, 0}
		n = 0
	}
	for i := 0; i < frames; i++ {
		for ch := 0; ch < 2; ch++ {
			x := float64(samples[i*2+ch]) / 32768.0
			z[ch] = a1*z[ch] + (1-a1)*x
			energy[ch] += z[ch] * z[ch]
		}
		n++
		if n == blockFrames {
			flush()
		}
	}
	flush()
	return blocks
}

// integrate applies the BS.1770 absolute then -10 LU relative gate.
func integrate(blocks []float64) (float64, int) {
	var kept []float64
	for _, l := range blocks {
		if l > absoluteGate {
			kept = append(kept, l)
		}
	}
	if len(kept) == 0 {
		return math.Inf(-1), 0
	}
	mean := avgDB(kept)
	var gated []float64
	for _, l := range blocks {
		if l > mean+relativeGate {
			gated = append(gated, l)
		}
	}
	if len(gated) == 0 {
		return math.Inf(-1), 0
	}
	return avgDB(gated), len(gated)
}

func avgDB(blocks []float64) float64 {
	sum := 0.0
	for _, l := range blocks {
		sum += math.Pow(10, (l+0.691)/10)
	}
	return -0.691 + 10*math.Log10(sum/float64(len(blocks)))
}
