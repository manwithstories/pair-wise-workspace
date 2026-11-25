package audio

import "math"

const silenceLUFS = -120.0

// LoudnessResult is the deterministic outcome of loudness/peak processing.
type LoudnessResult struct {
	Buffer         *Buffer
	InputLUFS      float64
	LUFS           float64
	TruePeakDBFS   float64
	GainDB         float64
	PeakTrimDB     float64
	Silent         bool
	ShortMaterial  bool
	PeakLimited    bool
	ClippedSamples int
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

// kWeightFilters returns the two BS.1770-style pre-filters. The coefficients
// are generated from the standard 48 kHz reference constants; callers convert
// to TargetRate first in the delivery pipeline.
func kWeightFilters(rate int) [2]biquad {
	if rate <= 0 {
		rate = TargetRate
	}
	// Stage 1: high shelf, gain about +4 dB.
	f0 := 1681.7697472016423
	gain := 3.999843853973347
	q := 0.7071752369554196
	k := math.Tan(math.Pi * f0 / float64(rate))
	vh := math.Pow(10, gain/20)
	vb := math.Pow(vh, 0.4996667741545416)
	a0 := 1 + k/q + k*k
	shelf := biquad{
		b0: (vh + vb*k/q + k*k) / a0,
		b1: 2 * (k*k - vh) / a0,
		b2: (vh - vb*k/q + k*k) / a0,
		a1: 2 * (k*k - 1) / a0,
		a2: (1 - k/q + k*k) / a0,
	}

	// Stage 2: high-pass.
	f0 = 38.13547087602189
	q = 0.5003270373238773
	k = math.Tan(math.Pi * f0 / float64(rate))
	a0 = 1 + k/q + k*k
	hp := biquad{
		b0: 1 / a0,
		b1: -2 / a0,
		b2: 1 / a0,
		a1: 2 * (k*k - 1) / a0,
		a2: (1 - k/q + k*k) / a0,
	}
	return [2]biquad{shelf, hp}
}

// MeasureLUFS computes deterministic K-weighted integrated loudness. Very
// short material uses the whole buffer rather than a nondeterministic gate.
func MeasureLUFS(b *Buffer) float64 {
	if b == nil || len(b.Samples) == 0 || b.Channels <= 0 {
		return silenceLUFS
	}
	filters := make([][2]biquad, b.Channels)
	sums := make([]float64, b.Channels)
	counts := make([]int, b.Channels)
	for f := 0; f < b.Frames(); f++ {
		for c := 0; c < b.Channels; c++ {
			x := b.Samples[f*b.Channels+c]
			if math.Abs(x) < 1e-12 {
				// Still feed filters below; this only supports silence detection.
			}
			fs := filters[c]
			x = fs[0].process(x)
			x = fs[1].process(x)
			filters[c] = fs
			if math.IsNaN(x) || math.IsInf(x, 0) {
				continue
			}
			sums[c] += x * x
			counts[c]++
		}
	}
	weighted := 0.0
	for c := range sums {
		if counts[c] == 0 {
			continue
		}
		mean := sums[c] / float64(counts[c])
		// Stereo/front channels have unity weight. Mono receives the same
		// deterministic weighting in this implementation.
		weighted += mean
	}
	if weighted <= 1e-12 {
		return silenceLUFS
	}
	lufs := -0.691 + 10*math.Log10(weighted)
	if lufs < silenceLUFS {
		return silenceLUFS
	}
	return lufs
}

// TruePeakAmplitude estimates inter-sample peaks with 4x Hermite oversampling.
func TruePeakAmplitude(b *Buffer) float64 {
	if b == nil || len(b.Samples) == 0 {
		return 0
	}
	peak := 0.0
	frames := b.Frames()
	for c := 0; c < b.Channels; c++ {
		at := func(i int) float64 {
			if i < 0 {
				i = 0
			} else if i >= frames {
				i = frames - 1
			}
			return b.Samples[i*b.Channels+c]
		}
		for i := 0; i < frames; i++ {
			x0, x1, x2, x3 := at(i-1), at(i), at(i+1), at(i+2)
			peak = math.Max(peak, math.Abs(x1))
			m1 := (x2 - x0) / 2
			m2 := (x3 - x1) / 2
			for _, t := range []float64{0.25, 0.5, 0.75} {
				t2, t3 := t*t, t*t*t
				h00 := 2*t3 - 3*t2 + 1
				h10 := t3 - 2*t2 + t
				h01 := -2*t3 + 3*t2
				h11 := t3 - t2
				y := h00*x1 + h10*m1 + h01*x2 + h11*m2
				peak = math.Max(peak, math.Abs(y))
			}
		}
	}
	return peak
}

// Normalize applies loudness gain and a true-peak ceiling. The returned buffer
// is always a distinct allocation.
func Normalize(in *Buffer, targetLUFS, ceilingDBFS float64, clippedSamples int) LoudnessResult {
	short := in != nil && in.Rate > 0 && float64(in.Frames())/float64(in.Rate) < 0.4
	if in == nil || len(in.Samples) == 0 {
		return LoudnessResult{LUFS: silenceLUFS, TruePeakDBFS: silenceLUFS,
			Silent: true, ShortMaterial: short, ClippedSamples: clippedSamples,
			Buffer: &Buffer{Rate: TargetRate, Channels: TargetChannels}}
	}
	inputLUFS := MeasureLUFS(in)
	if inputLUFS <= silenceLUFS+0.001 {
		out := &Buffer{Rate: in.Rate, Channels: in.Channels, Samples: make([]float64, len(in.Samples))}
		return LoudnessResult{Buffer: out, InputLUFS: inputLUFS, LUFS: silenceLUFS,
			TruePeakDBFS: silenceLUFS, Silent: true, ShortMaterial: short,
			ClippedSamples: clippedSamples}
	}

	out := &Buffer{Rate: in.Rate, Channels: in.Channels, Samples: append([]float64(nil), in.Samples...)}
	gainDB := targetLUFS - inputLUFS
	gain := DBFSToAmplitude(gainDB)
	for i := range out.Samples {
		out.Samples[i] *= gain
	}

	peakAmp := TruePeakAmplitude(out)
	peakTrimDB := 0.0
	limited := false
	// 0.01 dB measurement/floating-point safety margin.
	ceilingAmp := DBFSToAmplitude(ceilingDBFS - 0.01)
	if peakAmp > ceilingAmp {
		trim := ceilingAmp / peakAmp
		for i := range out.Samples {
			out.Samples[i] *= trim
		}
		peakTrimDB = AmplitudeToDBFS(trim)
		limited = true
	}

	finalLUFS := MeasureLUFS(out)
	finalPeakDB := AmplitudeToDBFS(TruePeakAmplitude(out))
	return LoudnessResult{
		Buffer: out, InputLUFS: inputLUFS, LUFS: finalLUFS, TruePeakDBFS: finalPeakDB,
		GainDB: gainDB, PeakTrimDB: peakTrimDB, PeakLimited: limited,
		ShortMaterial: short, ClippedSamples: clippedSamples,
	}
}
