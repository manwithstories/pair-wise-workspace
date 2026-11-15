package dsp

import (
	"fmt"
	"math"
)

const (
	preGainDB = 4.0
	preCutoff = 1500.0
	rlbCutoff = 38.13547087602444
	rlbQ      = 0.5003270373238773
)

var ref48k = [2]Coeffs{
	{
		B0: 1.53512485958697,
		B1: -2.69169618940638,
		B2: 1.19839281085285,
		A1: -1.69065929318241,
		A2: 0.73248077421585,
	},
	{
		B0: 1.0,
		B1: -2.0,
		B2: 1.0,
		A1: -1.99004745483398,
		A2: 0.99007225036621,
	},
}

func KWWeightingCoeffs(sampleRate float64) ([2]Coeffs, error) {
	coef := [2]Coeffs{
		HighShelf(sampleRate, preGainDB, preCutoff),
		HighPass(sampleRate, rlbCutoff, rlbQ),
	}
	for _, stage := range coef {
		if !stage.Stable() {
			return coef, fmt.Errorf("unstable K-weighting coefficients at %.0f Hz", sampleRate)
		}
	}
	if sampleRate == 48000 {
		for i, stage := range coef {
			if MaxCoeffDelta(stage, ref48k[i]) > 2e-4 {
				return coef, fmt.Errorf("48 kHz coefficients differ from BS.1770 reference: stage %d", i+1)
			}
		}
		return coef, nil
	}
	if sampleRate != 44100 && sampleRate != 96000 {
		return coef, fmt.Errorf("unsupported sample rate %.0f Hz", sampleRate)
	}
	const responseTolDB = 1.0
	for i := 0; i < 2; i++ {
		ref := ref48k[i]
		for f := 20.0; f <= 20000.0; f *= math.Pow(2, 1.0/12.0) {
			if f >= sampleRate/2 {
				break
			}
			got := 20 * math.Log10(coef[i].Magnitude(f/sampleRate))
			want := 20 * math.Log10(ref.Magnitude(f/48000))
			if math.Abs(got-want) > responseTolDB {
				return coef, fmt.Errorf("K-weighting response out of tolerance at %.1f Hz: got %.3f dB want %.3f dB",
					f, got, want)
			}
		}
	}
	return coef, nil
}

type KWeighter struct {
	stages [2]Biquad
}

func NewKWeighter(sampleRate float64) (*KWeighter, error) {
	coef, err := KWWeightingCoeffs(sampleRate)
	if err != nil {
		return nil, err
	}
	return &KWeighter{stages: [2]Biquad{*NewBiquad(coef[0]), *NewBiquad(coef[1])}}, nil
}

func (k *KWeighter) Process(x float64) float64 {
	return k.stages[1].Process(k.stages[0].Process(x))
}

func (k *KWeighter) Reset() {
	k.stages[0].Reset()
	k.stages[1].Reset()
}

func (k *KWeighter) ProcessBuffer(in []float64) []float64 {
	out := make([]float64, len(in))
	for i, value := range in {
		out[i] = k.Process(value)
	}
	return out
}

// ChannelWeights follows BS.1770: L/R/C=1.0, surrounds=1.41, LFE excluded.
func ChannelWeights(channels uint16, mask uint32) ([]float64, []bool, error) {
	weights := make([]float64, channels)
	used := make([]bool, channels)
	if mask != 0 {
		bits := []uint32{0x1, 0x2, 0x4, 0x8, 0x20, 0x40}
		for i := range weights {
			if i < len(bits) && mask&bits[i] != 0 {
				used[i] = true
				if bits[i] == 0x20 || bits[i] == 0x40 {
					weights[i] = 1.41
				} else if bits[i] == 0x8 {
					weights[i] = 0
				} else {
					weights[i] = 1
				}
			}
		}
		return weights, used, nil
	}
	switch channels {
	case 1:
		weights[0], used[0] = 1, true
	case 2:
		weights[0], weights[1] = 1, 1
		used[0], used[1] = true, true
	case 6:
		copy(weights, []float64{1, 1, 1, 0, 1.41, 1.41})
		for i := range used {
			used[i] = weights[i] > 0
		}
	default:
		return nil, nil, fmt.Errorf("unsupported channel layout without mask: %d channels", channels)
	}
	return weights, used, nil
}
