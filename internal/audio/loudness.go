package audio

import (
	"math"

	"mediad/internal/fault"
)

// LoudnessReport contains BS.1770-4-style integrated loudness and peak metrics.
type LoudnessReport struct {
	IntegratedLUFS float64 `json:"integrated_lufs"`
	UngatedLUFS    float64 `json:"ungated_lufs"`
	TruePeakDBTP   float64 `json:"true_peak_dbtp"`
	AppliedGainDB  float64 `json:"applied_gain_db,omitempty"`
	Blocks         int     `json:"blocks"`
}

type biquad struct {
	b0, b1, b2 float64
	a1, a2     float64
	x1, x2     float64
	y1, y2     float64
}

func (q *biquad) process(x float64) float64 {
	y := q.b0*x + q.b1*q.x1 + q.b2*q.x2 - q.a1*q.y1 - q.a2*q.y2
	q.x2, q.x1 = q.x1, x
	q.y2, q.y1 = q.y1, y
	return y
}

// kWeightFilters returns the BS.1770 K-weighting stages for 48 kHz.
func kWeightFilters() [2]biquad {
	shelf := biquad{
		b0: 1.53512485958697, b1: -2.69169618940638, b2: 1.19839281085285,
		a1: -1.69065929318241, a2: 0.73248077421585,
	}
	highpass := biquad{
		b0: 1, b1: -2, b2: 1,
		a1: -1.99004745483398, a2: 0.99007225036621,
	}
	return [2]biquad{shelf, highpass}
}

// KWeight applies the two K-weighting stages independently per channel.
func KWeight(b Buffer) Buffer {
	if b.SampleRate != 48000 {
		// The canonical master is always 48 kHz; meter arbitrary rates through
		// the same deterministic resampler so coefficients remain valid.
		rb, err := Resample(b, 48000)
		if err != nil {
			return b.Clone()
		}
		b = rb
	}
	out := b.Clone()
	for ch := 0; ch < b.Channels; ch++ {
		filters := kWeightFilters()
		for i := ch; i < len(b.Samples); i += b.Channels {
			x := b.Samples[i]
			for f := range filters {
				x = filters[f].process(x)
			}
			out.Samples[i] = x
		}
	}
	return out
}

func channelWeight(channels, channel int) float64 {
	if channels >= 5 {
		// Canonical order L,R,C,LFE,Ls,Rs; LFE is excluded.
		switch channel {
		case 3:
			return 0
		case 4, 5:
			return 1.4142135623730951
		}
	}
	return 1
}

func lufsFromEnergy(e float64) float64 {
	if e <= 1e-12 {
		return -math.Inf(1)
	}
	return -0.691 + 10*math.Log10(e)
}

// Measure computes gated integrated loudness and 4x-oversampled true peak.
func Measure(b Buffer) LoudnessReport {
	w := KWeight(b)
	rate := w.SampleRate
	blockFrames := int(0.4 * float64(rate))
	hopFrames := int(0.1 * float64(rate))
	if blockFrames <= 0 {
		blockFrames = 1
	}
	if hopFrames <= 0 {
		hopFrames = 1
	}
	frames := w.FrameCount()
	type block struct{ energy, lufs float64 }
	var blocks []block
	for start := 0; start+blockFrames <= frames; start += hopFrames {
		var energy float64
		for ch := 0; ch < w.Channels; ch++ {
			var sum float64
			n := 0
			for f := start; f < start+blockFrames; f++ {
				v := w.Samples[f*w.Channels+ch]
				sum += v * v
				n++
			}
			if n > 0 {
				energy += channelWeight(w.Channels, ch) * sum / float64(n)
			}
		}
		blocks = append(blocks, block{energy: energy, lufs: lufsFromEnergy(energy)})
	}

	wholeEnergy := func() float64 {
		var e float64
		for ch := 0; ch < w.Channels; ch++ {
			var sum float64
			n := 0
			for f := 0; f < frames; f++ {
				v := w.Samples[f*w.Channels+ch]
				sum += v * v
				n++
			}
			if n > 0 {
				e += channelWeight(w.Channels, ch) * sum / float64(n)
			}
		}
		return e
	}()

	if len(blocks) == 0 {
		lufs := lufsFromEnergy(wholeEnergy)
		return LoudnessReport{IntegratedLUFS: lufs, UngatedLUFS: lufs, TruePeakDBTP: TruePeakDBTP(b)}
	}

	var ungatedSum, ungatedN float64
	for _, blk := range blocks {
		if blk.lufs >= -70 {
			ungatedSum += blk.energy
			ungatedN++
		}
	}
	ungatedEnergy := wholeEnergy
	if ungatedN > 0 {
		ungatedEnergy = ungatedSum / ungatedN
	}
	ungatedLUFS := lufsFromEnergy(ungatedEnergy)
	relativeEnergy := math.Pow(10, (ungatedLUFS-10+0.691)/10)

	var integratedSum, integratedN float64
	for _, blk := range blocks {
		if blk.lufs >= -70 && blk.energy >= relativeEnergy {
			integratedSum += blk.energy
			integratedN++
		}
	}
	integratedEnergy := ungatedEnergy
	if integratedN > 0 {
		integratedEnergy = integratedSum / integratedN
	}
	return LoudnessReport{
		IntegratedLUFS: lufsFromEnergy(integratedEnergy),
		UngatedLUFS:    ungatedLUFS,
		TruePeakDBTP:   TruePeakDBTP(b),
		Blocks:         len(blocks),
	}
}

// TruePeakDBTP estimates 4x-oversampled true peak using cubic interpolation.
func TruePeakDBTP(b Buffer) float64 {
	var peak float64
	frames := b.FrameCount()
	for ch := 0; ch < b.Channels; ch++ {
		at := func(f int) float64 {
			if f < 0 {
				f = 0
			}
			if f >= frames {
				f = frames - 1
			}
			return b.Samples[f*b.Channels+ch]
		}
		for f := 0; f < frames; f++ {
			y0, y1, y2, y3 := at(f-1), at(f), at(f+1), at(f+2)
			for q := 0; q < 4; q++ {
				t := float64(q) / 4
				v := cubicHermite(y0, y1, y2, y3, t)
				peak = math.Max(peak, math.Abs(v))
			}
		}
	}
	if peak == 0 {
		return -math.Inf(1)
	}
	return 20 * math.Log10(peak)
}

func cubicHermite(y0, y1, y2, y3, t float64) float64 {
	// Catmull-Rom interpolation.
	a := -0.5*y0 + 1.5*y1 - 1.5*y2 + 0.5*y3
	b := y0 - 2.5*y1 + 2*y2 - 0.5*y3
	c := -0.5*y0 + 0.5*y2
	d := y1
	return ((a*t+b)*t+c)*t + d
}

// Master normalizes integrated loudness and enforces a true-peak ceiling.
func Master(in Buffer, targetLUFS, maxTruePeakDBTP float64) (Buffer, LoudnessReport, error) {
	if math.IsNaN(targetLUFS) || targetLUFS > 0 || targetLUFS < -70 {
		return Buffer{}, LoudnessReport{}, fault.New(fault.InvalidArgument, "", -1, "invalid target LUFS %.1f", targetLUFS)
	}
	out := in.Clone()
	before := Measure(out)
	if math.IsInf(before.IntegratedLUFS, -1) {
		return Buffer{}, LoudnessReport{}, fault.New(fault.MdatCorrupt, "", -1, "audio is digital silence; cannot normalize")
	}
	gainDB := targetLUFS - before.IntegratedLUFS
	gain := math.Pow(10, gainDB/20)
	for i := range out.Samples {
		out.Samples[i] *= gain
	}
	tp := TruePeakDBTP(out)
	if tp > maxTruePeakDBTP {
		extraDB := maxTruePeakDBTP - tp
		extra := math.Pow(10, extraDB/20)
		for i := range out.Samples {
			out.Samples[i] *= extra
		}
		gainDB += extraDB
	}
	after := Measure(out)
	after.AppliedGainDB = gainDB
	return out, after, nil
}
