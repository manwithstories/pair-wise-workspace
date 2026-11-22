package audio

import "math"

// Resampling uses a windowed-sinc low-pass. The kernel is symmetric and uses
// precomputed coefficients per output phase, so identical input always yields
// bit-identical output.

const sincTaps = 16 // taps per side of the kernel

type resampler struct {
	ratio  float64 // input samples per output sample
	cutoff float64 // normalised cutoff in input cycles/sample
	coeffs [][]float64
	phases int
}

func newResampler(inRate, outRate int) *resampler {
	if inRate <= 0 || outRate <= 0 || inRate == outRate {
		return nil
	}
	r := &resampler{ratio: float64(inRate) / float64(outRate), phases: 1024}
	// Anti-alias below the lower Nyquist frequency.
	cutoff := 0.5
	if outRate < inRate {
		cutoff = 0.5 * float64(outRate) / float64(inRate)
	}
	r.cutoff = cutoff
	r.coeffs = make([][]float64, r.phases)
	for p := 0; p < r.phases; p++ {
		frac := float64(p) / float64(r.phases)
		r.coeffs[p] = make([]float64, 2*sincTaps)
		var sum float64
		for k := -sincTaps + 1; k <= sincTaps; k++ {
			x := frac - float64(k)
			v := sinc(2*cutoff*x) * 2 * cutoff
			// Blackman window over the kernel support.
			w := blackman((float64(k) - frac + sincTaps) / (2 * sincTaps))
			v *= w
			r.coeffs[p][k+sincTaps-1] = v
			sum += v
		}
		if sum != 0 {
			for k := range r.coeffs[p] {
				r.coeffs[p][k] /= sum
			}
		}
	}
	return r
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	px := math.Pi * x
	return math.Sin(px) / px
}

func blackman(t float64) float64 {
	if t < 0 || t > 1 {
		return 0
	}
	return 0.42 - 0.5*math.Cos(2*math.Pi*t) + 0.08*math.Cos(4*math.Pi*t)
}

// process resamples one channel, returning the new frame count. Edge samples
// are zero-padded so the filter never reads outside the buffer.
func (r *resampler) process(in []float64, inFrames int, outFrames int) []float64 {
	out := make([]float64, outFrames)
	for n := 0; n < outFrames; n++ {
		pos := float64(n) * r.ratio
		base := int(math.Floor(pos))
		frac := pos - float64(base)
		phase := int(frac * float64(r.phases))
		if phase >= r.phases {
			phase = r.phases - 1
		}
		c := r.coeffs[phase]
		var acc float64
		for k := 0; k < len(c); k++ {
			idx := base + k - sincTaps + 1
			if idx >= 0 && idx < inFrames {
				acc += in[idx] * c[k]
			}
		}
		out[n] = acc
	}
	return out
}

// Resample converts interleaved samples from inRate to outRate, preserving
// the channel interleave. The output frame count is rounded half away from
// zero, keeping durations stable across repeated runs.
func Resample(samples []float64, channels, inRate, outRate int) ([]float64, int, error) {
	if channels <= 0 {
		return nil, 0, ErrInvalidLayout
	}
	if len(samples)%channels != 0 {
		return nil, 0, ErrInvalidLayout
	}
	inFrames := len(samples) / channels
	if inRate == outRate {
		return append([]float64(nil), samples...), inFrames, nil
	}
	if inRate <= 0 || outRate <= 0 {
		return nil, 0, ErrInvalidRate
	}
	r := newResampler(inRate, outRate)
	if r == nil {
		return append([]float64(nil), samples...), inFrames, nil
	}
	outFrames := int(math.Round(float64(inFrames) * float64(outRate) / float64(inRate)))
	out := make([]float64, outFrames*channels)
	channel := make([]float64, inFrames)
	for ch := 0; ch < channels; ch++ {
		for i := 0; i < inFrames; i++ {
			channel[i] = samples[i*channels+ch]
		}
		res := r.process(channel, inFrames, outFrames)
		for i := 0; i < outFrames; i++ {
			out[i*channels+ch] = res[i]
		}
	}
	return out, outFrames, nil
}

// Errors shared by layout and rate checks.
var (
	ErrInvalidLayout = errAudio("audio: invalid channel layout")
	ErrInvalidRate   = errAudio("audio: invalid sample rate")
)

type errAudio string

func (e errAudio) Error() string { return string(e) }
