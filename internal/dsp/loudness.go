package dsp

import (
	"errors"
	"math"
)

var errSilentBuffer = errors.New("dsp: cannot normalize silent or invalid audio")

// R128TargetLUFS is the EBU R128 integrated-loudness target.
const R128TargetLUFS = -23.0

// MaxTruePeakDBTP is the delivery true-peak ceiling.
const MaxTruePeakDBTP = -1.0

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

func newKWeightingChains(channels int) [][2]*biquad {
	chains := make([][2]*biquad, channels)
	for ch := 0; ch < channels; ch++ {
		// RBJ coefficients for the first K-weighting high shelf.
		shelf := &biquad{
			b0: 1.53512485958697,
			b1: -2.69169618940638,
			b2: 1.19839281085285,
			a1: -1.69065929318241,
			a2: 0.73248077421585,
		}
		// Second K-weighting high-pass stage.
		highpass := &biquad{
			b0: 1.0,
			b1: -2.0,
			b2: 1.0,
			a1: -1.99004745483398,
			a2: 0.99007225036621,
		}
		chains[ch] = [2]*biquad{shelf, highpass}
	}
	return chains
}

// KWeight applies BS.1770 K-weighting and returns a new buffer.
func KWeight(in *Buffer) *Buffer {
	out := &Buffer{Rate: in.Rate, Channels: in.Channels, Interleaved: make([]float64, len(in.Interleaved))}
	chains := newKWeightingChains(in.Channels)
	for f := 0; f < in.Frames(); f++ {
		for ch := 0; ch < in.Channels; ch++ {
			i := f*in.Channels + ch
			y := chains[ch][0].process(in.Interleaved[i])
			y = chains[ch][1].process(y)
			out.Interleaved[i] = y
		}
	}
	return out
}

func loudnessFromPower(p float64) float64 {
	if p <= 1e-12 {
		return -math.Inf(1)
	}
	return -0.691 + 10*math.Log10(p)
}

func powerFromLUFS(lu float64) float64 {
	return math.Pow(10, (lu+0.691)/10)
}

// IntegratedLUFS computes gated BS.1770/EBU R128 integrated loudness.
func IntegratedLUFS(in *Buffer) float64 {
	frames := in.Frames()
	if frames == 0 {
		return -math.Inf(1)
	}
	weighted := KWeight(in)
	block := in.Rate * 4 / 10
	hop := in.Rate / 10
	if block <= 0 || hop <= 0 {
		return -math.Inf(1)
	}

	var powers []float64
	if frames < block {
		powers = append(powers, blockPower(weighted, 0, frames))
	} else {
		for start := 0; start+block <= frames; start += hop {
			powers = append(powers, blockPower(weighted, start, block))
		}
	}

	absGate := powerFromLUFS(-70)
	var absolute []float64
	for _, p := range powers {
		if p >= absGate {
			absolute = append(absolute, p)
		}
	}
	if len(absolute) == 0 {
		return -math.Inf(1)
	}
	mean := meanPower(absolute)
	relativeGate := powerFromLUFS(loudnessFromPower(mean) - 10)
	var gated []float64
	for _, p := range absolute {
		if p >= relativeGate {
			gated = append(gated, p)
		}
	}
	if len(gated) == 0 {
		gated = absolute
	}
	return loudnessFromPower(meanPower(gated))
}

func blockPower(b *Buffer, start, count int) float64 {
	var total float64
	for ch := 0; ch < b.Channels; ch++ {
		var sum float64
		for f := start; f < start+count; f++ {
			v := b.Interleaved[f*b.Channels+ch]
			sum += v * v
		}
		total += sum / float64(count)
	}
	return total
}

func meanPower(ps []float64) float64 {
	var sum float64
	for _, p := range ps {
		sum += p
	}
	return sum / float64(len(ps))
}

// NormalizationResult records deterministic gain and meter readings.
type NormalizationResult struct {
	InputLUFS    float64
	OutputLUFS   float64
	GainDB       float64
	TruePeakDBTP float64
	Corrections  int
}

// NormalizeToR128 normalizes to target integrated loudness and enforces a
// true-peak ceiling. It returns a new buffer and before-quantization meter
// values.
func NormalizeToR128(in *Buffer, targetLUFS, truePeakCeiling float64) (*Buffer, NormalizationResult, error) {
	out := in.Clone()
	inputLU := IntegratedLUFS(out)
	if math.IsInf(inputLU, -1) || math.IsNaN(inputLU) {
		return out, NormalizationResult{InputLUFS: inputLU}, errSilentBuffer
	}

	gainDB := targetLUFS - inputLU
	ApplyDB(out, gainDB)
	res := NormalizationResult{InputLUFS: inputLU, GainDB: gainDB}

	for i := 0; i < 4; i++ {
		lu := IntegratedLUFS(out)
		errDB := targetLUFS - lu
		if math.Abs(errDB) <= 0.03 {
			break
		}
		ApplyDB(out, errDB)
		res.GainDB += errDB
		res.Corrections++
	}

	tp := TruePeakDBTP(out)
	if tp > truePeakCeiling {
		extra := truePeakCeiling - tp
		ApplyDB(out, extra)
		res.GainDB += extra
		tp = TruePeakDBTP(out)
	}
	res.OutputLUFS = IntegratedLUFS(out)
	res.TruePeakDBTP = tp
	return out, res, nil
}

// TruePeakDBTP estimates four-times oversampled true peak with a windowed-sinc
// interpolator.
func TruePeakDBTP(in *Buffer) float64 {
	const radius = 10
	peak := 0.0
	for ch := 0; ch < in.Channels; ch++ {
		for n := 0; n < in.Frames(); n++ {
			for phase := 0; phase < 4; phase++ {
				pos := float64(n) + float64(phase)/4.0
				center := int(math.Floor(pos + 0.5))
				var sum, weight float64
				for k := center - radius; k <= center+radius; k++ {
					t := pos - float64(k)
					w := sinc(t) * hann(0.5*(1-math.Abs(t)/radius))
					idx := clampIndex(k, in.Frames())
					v := in.Interleaved[idx*in.Channels+ch]
					sum += w * v
					weight += w
				}
				if weight != 0 {
					sum /= weight
				}
				if a := math.Abs(sum); a > peak {
					peak = a
				}
			}
		}
	}
	if peak < 1e-7 {
		return -140
	}
	db := 20 * math.Log10(peak)
	if db < -140 {
		return -140
	}
	return db
}
