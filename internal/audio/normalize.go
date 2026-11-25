// Package audio normalizes PCM across cameras: channel layout mapping
// (5.1 <-> stereo) and sample-rate conversion (44.1 <-> 48 kHz), with
// cut points checked against sample-block boundaries.
package audio

import (
	"errors"
	"fmt"

	"fieldqc/internal/clock"
	"fieldqc/internal/fault"
)

// Channel layouts supported by Normalize.
const (
	LayoutStereo = 2
	Layout51     = 6 // FL FR FC LFE SL SR
)

// PCM is interleaved signed 16-bit samples.
type PCM struct {
	SampleRate int
	Channels   int
	Samples    []int16
}

// Frames returns the number of sample frames.
func (p *PCM) Frames() int {
	if p.Channels <= 0 {
		return 0
	}
	return len(p.Samples) / p.Channels
}

func clamp16(v int64) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

// Downmix51 maps 5.1 (FL FR FC LFE SL SR) to stereo with 0.707 (-3 dB)
// center and surround coefficients; LFE is dropped.
func Downmix51(in []int16) []int16 {
	out := make([]int16, len(in)/3)
	for i, o := 0, 0; i+6 <= len(in); i, o = i+6, o+2 {
		l := int64(in[i]) + int64(in[i+2])*707/1000 + int64(in[i+4])*707/1000
		r := int64(in[i+1]) + int64(in[i+2])*707/1000 + int64(in[i+5])*707/1000
		out[o], out[o+1] = clamp16(l), clamp16(r)
	}
	return out
}

// Resample converts interleaved samples between rates (e.g. 44100 ->
// 48000) using exact rational positions and linear interpolation.
func Resample(in []int16, channels, from, to int) []int16 {
	if from == to {
		return append([]int16(nil), in...)
	}
	frames := len(in) / channels
	outFrames := int(int64(frames) * int64(to) / int64(from))
	out := make([]int16, outFrames*channels)
	for f := 0; f < outFrames; f++ {
		num := int64(f) * int64(from)
		s0 := int(num / int64(to))
		rem := num % int64(to)
		s1 := s0 + 1
		if s1 >= frames {
			s1 = frames - 1
		}
		for c := 0; c < channels; c++ {
			a := int64(in[s0*channels+c])
			b := int64(in[s1*channels+c])
			out[f*channels+c] = clamp16(a + (b-a)*rem/int64(to))
		}
	}
	return out
}

// Normalize maps the layout to targetCh and resamples to targetRate.
func Normalize(p *PCM, targetRate, targetCh int) (*PCM, error) {
	if p.Channels != targetCh {
		switch {
		case p.Channels == Layout51 && targetCh == LayoutStereo:
			p = &PCM{p.SampleRate, LayoutStereo, Downmix51(p.Samples)}
		default:
			return nil, fmt.Errorf("audio: unsupported channel mapping %d -> %d", p.Channels, targetCh)
		}
	}
	if p.SampleRate != targetRate {
		if p.SampleRate <= 0 || targetRate <= 0 {
			return nil, errors.New("audio: invalid sample rate")
		}
		p = &PCM{targetRate, p.Channels, Resample(p.Samples, p.Channels, p.SampleRate, targetRate)}
	}
	return p, nil
}

// Edge describes a cut point that missed a sample-block boundary.
type Edge struct {
	CutFrame int
	Block    int
	Pad      int // frames of padding (补齐) to close the partial block
	Gap      int // frames missing (缺口) beyond available media
}

// Cut splits p at frame. A cut inside a block records padding; a cut
// beyond the media records a gap.
func Cut(p *PCM, frame, block int) (before, after *PCM, e Edge) {
	frames := p.Frames()
	e = Edge{CutFrame: frame, Block: block}
	cut := frame
	if cut > frames {
		e.Gap = cut - frames
		cut = frames
	}
	if cut < 0 {
		cut = 0
	}
	if block > 0 {
		if rem := cut % block; rem != 0 {
			e.Pad = block - rem
		}
	}
	before = &PCM{p.SampleRate, p.Channels, append([]int16(nil), p.Samples[:cut*p.Channels]...)}
	after = &PCM{p.SampleRate, p.Channels, append([]int16(nil), p.Samples[cut*p.Channels:]...)}
	return before, after, e
}

// EdgeSpan converts an Edge to a fault span on the shared clock; ok is
// false when the cut landed exactly on a block boundary.
func EdgeSpan(track string, rate int, e Edge) (fault.Span, bool) {
	if e.Pad == 0 && e.Gap == 0 {
		return fault.Span{}, false
	}
	start, err := clock.Ticks(int64(e.CutFrame), int64(rate))
	if err != nil {
		return fault.Span{}, false
	}
	end, _ := clock.Ticks(int64(e.CutFrame+e.Pad+e.Gap), int64(rate))
	return fault.Span{
		Track: track, Code: fault.CodeAudioEdge, Start: start, End: end,
		Detail: fmt.Sprintf("cut at frame %d off %d-block: pad=%d gap=%d", e.CutFrame, e.Block, e.Pad, e.Gap),
	}, true
}
