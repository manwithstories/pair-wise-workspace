// Package audio normalizes recovered audio to the archive target:
// 48 kHz, stereo, integrated loudness -23 LUFS.
package audio

import "fmt"

// TargetRate and TargetChannels are fixed by the archive specification.
const (
	TargetRate     = 48000
	TargetChannels = 2
	TargetLUFS     = -23.0
)

// Frame is interleaved PCM in signed 16-bit samples.
type Frame struct {
	SampleRate int
	Channels   int
	Samples    []int16 // interleaved, length = frame count * Channels
}

// minBoundaryFrames is the tail that must remain for a safe resample filter.
// Shorter tails are preserved verbatim with a reported reason instead of
// producing a speculative partial conversion.
const minBoundaryFrames = 4

// Convert resamples to 48 kHz and up/down-mixes to stereo with linear
// interpolation. It is deterministic and allocation-bounded by input size.
func Convert(f Frame) (Frame, BoundaryReport, error) {
	if f.Channels < 1 || len(f.Samples)%f.Channels != 0 {
		return Frame{}, BoundaryReport{}, fmt.Errorf("audio: malformed frame: ch=%d n=%d", f.Channels, len(f.Samples))
	}
	frames := len(f.Samples) / f.Channels
	ratio := float64(TargetRate) / float64(f.SampleRate)
	outFrames := int(float64(frames) * ratio)
	// Keep an under-sized boundary fragment original rather than filtering
	// across a truncation point we cannot see.
	if frames < minBoundaryFrames && f.SampleRate != TargetRate {
		return f, BoundaryReport{Preserved: true, Reason: "boundary shorter than resample filter tail"}, nil
	}
	stereo := toStereo(f.Samples, f.Channels, frames)
	if f.SampleRate == TargetRate {
		return Frame{SampleRate: TargetRate, Channels: TargetChannels, Samples: stereo}, BoundaryReport{}, nil
	}
	out := make([]int16, outFrames*TargetChannels)
	for i := 0; i < outFrames; i++ {
		srcPos := float64(i) / ratio
		i0 := int(srcPos)
		frac := srcPos - float64(i0)
		i1 := i0 + 1
		if i1 >= frames {
			i1 = frames - 1
		}
		for ch := 0; ch < TargetChannels; ch++ {
			a := stereo[i0*TargetChannels+ch]
			b := stereo[i1*TargetChannels+ch]
			out[i*TargetChannels+ch] = lerp16(a, b, frac)
		}
	}
	return Frame{SampleRate: TargetRate, Channels: TargetChannels, Samples: out}, BoundaryReport{}, nil
}

// toStereo maps mono to dual mono and >2-channel layouts to L/R by standard
// down-mix weights (center shared, surrounds attenuated 3 dB).
func toStereo(in []int16, ch, frames int) []int16 {
	out := make([]int16, frames*TargetChannels)
	switch ch {
	case 1:
		for i := 0; i < frames; i++ {
			out[i*2], out[i*2+1] = in[i], in[i]
		}
	case 2:
		copy(out, in)
	default:
		for i := 0; i < frames; i++ {
			base := i * ch
			l := int32(in[base])
			r := int32(in[base+1])
			if ch >= 3 { // center
				c := int32(in[base+2]) / 2
				l += c
				r += c
			}
			for j := 3; j < ch; j++ { // surrounds, -3 dB
				s := int32(float64(in[base+j]) * 0.3536)
				l += s / 2
				r += s / 2
			}
			out[i*2] = clamp16(l)
			out[i*2+1] = clamp16(r)
		}
	}
	return out
}

func lerp16(a, b int16, frac float64) int16 {
	v := float64(a)*(1-frac) + float64(b)*frac
	return clamp16(int32(v + 0.5))
}

func clamp16(v int32) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

// BoundaryReport explains why a fragment could not be fully converted.
type BoundaryReport struct {
	Preserved bool
	Reason    string
}
