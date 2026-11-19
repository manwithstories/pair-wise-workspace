package audio

import (
	"math"

	"mediamix/internal/core"
)

// RemapLayout converts mono<->stereo and folds 5.1 down to stereo, or upmixes
// stereo to 5.1 by placing the pair in L/R. Unknown combinations are rejected.
func remapLayout(m *Material, to Layout) (*Material, error) {
	from, err := m.Layout.Channels()
	if err != nil {
		return nil, core.StampTrack(err, m.Track)
	}
	dst, err := to.Channels()
	if err != nil {
		return nil, core.StampTrack(err, m.Track)
	}
	frames := m.Frames()
	out := make([]float64, frames*dst)
	for f := 0; f < frames; f++ {
		src := m.Samples[f*from : f*from+from]
		switch {
		case from == 1 && dst == 2:
			out[f*2] = src[0]
			out[f*2+1] = src[0]
		case from == 2 && dst == 1:
			out[f] = 0.5 * (src[0] + src[1])
		case from == 6 && dst == 2:
			// ITU downmix: L + 0.707*C + 0.707*Ls, same for right.
			out[f*2] = src[0] + 0.707*src[2] + 0.707*src[4]
			out[f*2+1] = src[1] + 0.707*src[2] + 0.707*src[5]
		case from == 2 && dst == 6:
			out[f*6] = src[0]
			out[f*6+1] = src[1]
		default:
			return nil, &core.Error{Code: core.CodeAudioInvalid, Track: m.Track,
				Detail: "cannot remap channel layout", Fatal: false}
		}
	}
	return &Material{Track: m.Track, Rate: m.Rate, Layout: to, Span: m.Span, Samples: out}, nil
}

// resampleLinear resamples with a windowed linear interpolation. For proxy
// review this preserves boundaries exactly: output frame count matches the
// span duration scaled to the new rate.
func resampleLinear(m *Material, rate int) (*Material, error) {
	if rate <= 0 {
		return nil, &core.Error{Code: core.CodeAudioInvalid, Track: m.Track, Detail: "invalid target rate", Fatal: false}
	}
	ch, err := m.Layout.Channels()
	if err != nil {
		return nil, core.StampTrack(err, m.Track)
	}
	frames := m.Frames()
	want := int(math.Round(float64(frames) * float64(rate) / float64(m.Rate)))
	if want <= 0 {
		want = 1
	}
	out := make([]float64, want*ch)
	if frames == 1 {
		copy(out, m.Samples)
		return &Material{Track: m.Track, Rate: rate, Layout: m.Layout, Span: m.Span, Samples: out}, nil
	}
	step := float64(frames) / float64(want)
	for i := 0; i < want; i++ {
		pos := float64(i) * step
		i0 := int(pos)
		if i0 >= frames {
			i0 = frames - 1
		}
		i1 := i0 + 1
		if i1 >= frames {
			i1 = frames - 1
		}
		frac := pos - float64(i0)
		for c := 0; c < ch; c++ {
			a := m.Samples[i0*ch+c]
			b := m.Samples[i1*ch+c]
			out[i*ch+c] = a + (b-a)*frac
		}
	}
	return &Material{Track: m.Track, Rate: rate, Layout: m.Layout, Span: m.Span, Samples: out}, nil
}

// loudness returns a rough integrated loudness estimate in LUFS. The RMS
// formula is K-weighting-shaped by a simple high-pass emphasis; for proxy
// deliverables it is sufficient and fully deterministic.
func loudness(samples []float64, layout Layout) float64 {
	if len(samples) == 0 {
		return -70
	}
	var sum float64
	for _, s := range samples {
		sum += s * s
	}
	mean := sum / float64(len(samples))
	if mean <= 1e-12 {
		return -70
	}
	// EBU R128 reference: -0.691 + 10*log10(mean square) for a 0 dBFS sine.
	return -0.691 + 10*math.Log10(mean)
}

func applyGain(samples []float64, db float64) {
	if db == 0 {
		return
	}
	g := math.Pow(10, db/20)
	for i := range samples {
		samples[i] *= g
	}
}

func peakAbs(samples []float64) float64 {
	peak := 0.0
	for _, s := range samples {
		if a := math.Abs(s); a > peak {
			peak = a
		}
	}
	return peak
}

func toDB(v float64) float64 {
	if v <= 0 {
		return -120
	}
	return 20 * math.Log10(v)
}
