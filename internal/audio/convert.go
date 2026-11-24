package audio

import (
	"math"

	"mediarig/internal/media"
)

// PolyphaseFIR is an exact-phase rational resampler. Output sample k maps to
// source coordinate k*inRate/outRate; the reduced denominator selects one FIR
// phase, so sample positions never accumulate floating-point error.
type PolyphaseFIR struct {
	InRate  int
	OutRate int
	P       int64
	Q       int64
	Half    int
	Taps    int
	Coeffs  [][]float64
}

func gcdInt64(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func NewPolyphaseFIR(inRate, outRate int) (*PolyphaseFIR, error) {
	if inRate <= 0 || outRate <= 0 {
		return nil, media.Internal(media.StageAudio, "invalid resampler rates %d->%d", inRate, outRate)
	}
	g := gcdInt64(int64(inRate), int64(outRate))
	p := int64(inRate) / g
	q := int64(outRate) / g
	half := 16
	taps := half*2 + 1
	f := &PolyphaseFIR{InRate: inRate, OutRate: outRate, P: p, Q: q, Half: half, Taps: taps}
	f.Coeffs = make([][]float64, q)
	cutoff := math.Min(float64(inRate), float64(outRate)) * 0.45 / float64(inRate)
	if cutoff > 1 {
		cutoff = 1
	}
	for phase := int64(0); phase < q; phase++ {
		frac := float64(phase) / float64(q)
		co := make([]float64, taps)
		sum := 0.0
		for t := -half; t <= half; t++ {
			x := float64(t) - frac
			w := 0.5 - 0.5*math.Cos(2*math.Pi*(x+float64(half))/float64(taps-1))
			sinc := 1.0
			if x != 0 {
				sinc = math.Sin(math.Pi*cutoff*x) / (math.Pi * cutoff * x)
			}
			co[t+half] = sinc * w * cutoff
			sum += co[t+half]
		}
		for i := range co {
			co[i] /= sum
		}
		f.Coeffs[phase] = co
	}
	return f, nil
}

// Position returns floor(source coordinate) and exact polyphase index.
func (f *PolyphaseFIR) Position(outIndex int64) (center int64, phase int) {
	num := outIndex * f.P
	center = num / f.Q
	rem := num % f.Q
	if rem < 0 {
		rem += f.Q
		center--
	}
	return center, int(rem)
}

func (f *PolyphaseFIR) interpolate(ch []float64, outIndex int64) float64 {
	center, phase := f.Position(outIndex)
	co := f.Coeffs[phase]
	var sum float64
	for t := -f.Half; t <= f.Half; t++ {
		idx := center + int64(t)
		if idx < 0 || idx >= int64(len(ch)) {
			continue
		}
		sum += ch[idx] * co[t+f.Half]
	}
	return sum
}

// Resample converts all channels and preserves an exact integer output frame
// count obtained by rational duration conversion. Edge padding compensates
// FIR delay; positions at the tail are evaluated with zero padding, which
// flushes the finite filter ring without changing the timeline duration.
func Resample(in Audio, outRate int) (Audio, error) {
	if !IsSupportedRate(outRate) {
		return Audio{}, media.Policy(media.StageAudio, "unsupported output sample rate %d", outRate)
	}
	if in.SampleRate == outRate {
		in.Layout = LayoutStereo
		return in, nil
	}
	if !IsSupportedRate(in.SampleRate) {
		return Audio{}, media.Policy(media.StageAudio, "unsupported input sample rate %d", in.SampleRate)
	}
	fir, err := NewPolyphaseFIR(in.SampleRate, outRate)
	if err != nil {
		return Audio{}, err
	}
	frames := in.Frames()
	duration := media.SamplesToSeconds(int64(frames), in.SampleRate)
	outFrames := int(media.SecondsToSamples(duration, outRate))
	out := NewAudio(outRate, len(in.Channels), outFrames)
	for c := range in.Channels {
		for k := 0; k < outFrames; k++ {
			out.Channels[c][k] = fir.interpolate(in.Channels[c], int64(k))
		}
	}
	out.Layout = LayoutStereo
	return out, nil
}
