package dsp

import (
	"fmt"

	"mediad/internal/clock"
	"mediad/internal/pcm"
)

// Resample converts 44.1 or 48 kHz audio to the canonical 48 kHz rate.
//
// The interpolator is deterministic integer-indexed linear interpolation.
// Output duration is rounded to the nearest frame; generated fixtures have an
// integral rational duration and therefore produce the exact required frame
// count.
func Resample(in *pcm.Audio, outRate int) (*pcm.Audio, error) {
	if in == nil {
		return nil, fmt.Errorf("nil audio")
	}
	if in.SampleRate != clock.Rate44100 && in.SampleRate != clock.Rate48000 {
		return nil, fmt.Errorf("unsupported input rate %d", in.SampleRate)
	}
	if outRate != clock.Rate48000 {
		return nil, fmt.Errorf("unsupported output rate %d", outRate)
	}
	if in.SampleRate == outRate {
		cp := in.Clone()
		return cp, nil
	}

	outFrames := int((int64(in.Frames)*int64(outRate) + int64(in.SampleRate)/2) / int64(in.SampleRate))
	out, err := pcm.NewAudio(outRate, in.Channels, outFrames)
	if err != nil {
		return nil, err
	}

	inRate := int64(in.SampleRate)
	outRate64 := int64(outRate)
	for i := 0; i < outFrames; i++ {
		// Source position = i * inRate / outRate, kept as integer base/fraction.
		scaled := int64(i) * inRate
		base := scaled / outRate64
		rem := scaled % outRate64
		for ch := 0; ch < in.Channels; ch++ {
			var v float64
			if base >= int64(in.Frames-1) {
				v = in.Samples[(in.Frames-1)*in.Channels+ch]
			} else {
				a := in.Samples[int(base)*in.Channels+ch]
				b := in.Samples[(int(base)+1)*in.Channels+ch]
				frac := float64(rem) / float64(outRate64)
				v = a + (b-a)*frac
			}
			out.Samples[i*in.Channels+ch] = v
		}
	}
	return out, nil
}
