package audio

import (
	"math"
)

// Loudness is measured with an ITU-R BS.1770 style K-weighting filter and
// gated 400 ms blocks. Only the relative gain matters for delivery, so the
// absolute calibration constant is fixed and documented rather than
// hardware-dependent.

// kWeighting holds the two biquad stages of the K-weighting filter.
type kWeighting struct {
	b0, b1, b2, a1, a2 float64
}

// shelfFilter returns stage 1 of the K-weighting filter for the given rate.
// Coefficients follow the RBJ high-shelf formulation with the BS.1770
// prototype frequency and gain, so other rates use the same pre-warping.
func shelfFilter(rate int) kWeighting {
	// Stage 1: high shelf (+4 dB above ~1.5 kHz).
	f0 := 1681.974450955533
	gainDB := 3.999843853973347
	q := 0.7071752369554196
	a := math.Pow(10, gainDB/40)
	w0 := 2 * math.Pi * f0 / float64(rate)
	alpha := math.Sin(w0) / (2 * q)
	cosw := math.Cos(w0)
	a0 := (a + 1) - (a-1)*cosw + 2*math.Sqrt(a)*alpha
	shelf := kWeighting{
		b0: a * ((a + 1) + (a-1)*cosw + 2*math.Sqrt(a)*alpha),
		b1: -2 * a * ((a - 1) + (a+1)*cosw),
		b2: a * ((a + 1) + (a-1)*cosw - 2*math.Sqrt(a)*alpha),
		a1: 2 * ((a - 1) - (a+1)*cosw),
		a2: (a + 1) - (a-1)*cosw - 2*math.Sqrt(a)*alpha,
	}
	shelf.b0, shelf.b1, shelf.b2, shelf.a1, shelf.a2 =
		shelf.b0/a0, shelf.b1/a0, shelf.b2/a0, shelf.a1/a0, shelf.a2/a0
	return shelf
}

// biquadHPK is stage 2 of the K-weighting filter.
func biquadHPK(rate int) kWeighting {
	f0 := 38.13547087602444
	q := 0.5003270373253953
	w0 := 2 * math.Pi * f0 / float64(rate)
	alpha := math.Sin(w0) / (2 * q)
	cosw := math.Cos(w0)
	b0 := (1 + cosw) / 2
	b1 := -(1 + cosw)
	b2 := (1 + cosw) / 2
	a0 := 1 + alpha
	a1 := -2 * cosw
	a2 := 1 - alpha
	return kWeighting{b0: b0 / a0, b1: b1 / a0, b2: b2 / a0, a1: a1 / a0, a2: a2 / a0}
}

// apply runs a biquad over one channel in place.
func (f kWeighting) apply(x []float64) {
	var z1, z2 float64
	for i, v := range x {
		y := f.b0*v + z1
		z1 = f.b1*v - f.a1*y + z2
		z2 = f.b2*v - f.a2*y
		x[i] = y
	}
}

// blockSize is 400 ms.
func blockSize(rate int) int { return rate * 4 / 10 }

// LoudnessLUFS measures integrated loudness in LUFS for interleaved samples.
// Returns -inf when the signal is silent.
func LoudnessLUFS(samples []float64, rate, channels int) float64 {
	if rate <= 0 || channels <= 0 || len(samples) < channels {
		return math.Inf(-1)
	}
	frames := len(samples) / channels
	// K-weight each channel.
	weighted := make([]float64, len(samples))
	copy(weighted, samples)
	shelf := shelfFilter(rate)
	hpk := biquadHPK(rate)
	channel := make([]float64, frames)
	for ch := 0; ch < channels; ch++ {
		for i := 0; i < frames; i++ {
			channel[i] = weighted[i*channels+ch]
		}
		shelf.apply(channel)
		hpk.apply(channel)
		for i := 0; i < frames; i++ {
			weighted[i*channels+ch] = channel[i]
		}
	}
	// Gated blocks: absolute gate -70 LUFS, relative gate -10 LU.
	bs := blockSize(rate)
	if bs <= 0 || frames < bs {
		return singleBlockLoudness(weighted, frames, channels)
	}
	blocks := frames / bs
	energies := make([]float64, 0, blocks)
	for b := 0; b < blocks; b++ {
		var sum float64
		for i := b * bs; i < (b+1)*bs; i++ {
			for ch := 0; ch < channels; ch++ {
				v := weighted[i*channels+ch]
				sum += v * v
			}
		}
		mean := sum / float64(bs*channels)
		if mean <= 0 {
			continue
		}
		l := -0.691 + 10*math.Log10(mean)
		if l > -70 {
			energies = append(energies, mean)
		}
	}
	if len(energies) == 0 {
		return math.Inf(-1)
	}
	var total float64
	for _, e := range energies {
		total += e
	}
	relMean := total / float64(len(energies))
	relGate := -0.691 + 10*math.Log10(relMean) - 10
	var gatedSum float64
	var gatedCount int
	for _, e := range energies {
		l := -0.691 + 10*math.Log10(e)
		if l > relGate {
			gatedSum += e
			gatedCount++
		}
	}
	if gatedCount == 0 {
		return math.Inf(-1)
	}
	return -0.691 + 10*math.Log10(gatedSum/float64(gatedCount))
}

func singleBlockLoudness(samples []float64, frames, channels int) float64 {
	var sum float64
	for i := 0; i < frames*channels; i++ {
		sum += samples[i] * samples[i]
	}
	mean := sum / float64(frames*channels)
	if mean <= 0 {
		return math.Inf(-1)
	}
	return -0.691 + 10*math.Log10(mean)
}

// PeakDBFS returns the maximum absolute sample value in dBFS.
func PeakDBFS(samples []float64) float64 {
	var peak float64
	for _, v := range samples {
		a := math.Abs(v)
		if a > peak {
			peak = a
		}
	}
	if peak <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(peak)
}

// GainForTarget computes the linear gain to reach target LUFS without
// exceeding the peak ceiling.
func GainForTarget(measured, target, peakDBFS, ceilingDBFS float64) float64 {
	if math.IsInf(measured, -1) {
		return 1
	}
	gainDB := target - measured
	if !math.IsInf(peakDBFS, -1) && peakDBFS+gainDB > ceilingDBFS {
		gainDB = ceilingDBFS - peakDBFS
	}
	return math.Pow(10, gainDB/20)
}

// ApplyGain scales samples in place and returns the applied gain in dB.
func ApplyGain(samples []float64, gain float64) float64 {
	for i := range samples {
		samples[i] *= gain
	}
	if gain <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(gain)
}
