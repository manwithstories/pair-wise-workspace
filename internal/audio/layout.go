// Package audio normalizes PCM material: sample-rate conversion, explicit
// channel-layout downmix and loudness/true-peak control.
package audio

import (
	"fmt"

	"github.com/fieldcut/fieldcut/internal/base"
)

// Layout names the supported source channel orders (SMPTE/ITU order).
type Layout string

const (
	LayoutMono   Layout = "mono"
	LayoutStereo Layout = "stereo"
	Layout51     Layout = "5.1"
)

// Channel count per layout.
func (l Layout) Channels() int {
	switch l {
	case LayoutMono:
		return 1
	case LayoutStereo:
		return 2
	case Layout51:
		return 6
	}
	return 0
}

// Valid reports a known layout.
func (l Layout) Valid() bool { return l.Channels() > 0 }

// 5.1 order: L, R, C, LFE, Ls, Rs.
const (
	chL = iota
	chR
	chC
	chLFE
	chLs
	chRs
)

// Downmix gains follow ITU-R BS.775 Annex 1 coefficients. LFE carries no
// stereo equivalent and is measured but intentionally not folded in, which
// is logged rather than silently dropped.
const (
	gainCenter = 0.70710678 // -3 dB
	gainSurround = 0.70710678
)

// Frame is one PCM block: interleaved float32 samples in [-1, 1].
type Frame struct {
	SampleRate uint32
	Layout     Layout
	Samples    []float32 // interleaved, len = frames * Channels()
}

// Downmix converts any supported layout to stereo using an explicit,
// documented per-channel mapping. The returned LFE level lets callers record
// that the discrete LFE path was not mixed into the stereo bus.
func Downmix(f *Frame) (stereo []float32, lfePeak float32, err error) {
	if !f.Layout.Valid() {
		return nil, 0, base.NewFault(base.FaultBoundary, base.Locator{}, false,
			"unknown channel layout %q", f.Layout)
	}
	ch := f.Layout.Channels()
	if len(f.Samples)%ch != 0 {
		return nil, 0, fmt.Errorf("pcm samples %d not divisible by %d channels", len(f.Samples), ch)
	}
	frames := len(f.Samples) / ch
	out := make([]float32, frames*2)
	for n := 0; n < frames; n++ {
		in := f.Samples[n*ch : (n+1)*ch]
		var l, r float32
		switch f.Layout {
		case LayoutMono:
			// Dual-mono: identical, decorrelated content on both speakers.
			l, r = in[0], in[0]
		case LayoutStereo:
			l, r = in[chL], in[chR]
		case Layout51:
			l = in[chL] + gainCenter*in[chC] + gainSurround*in[chLs]
			r = in[chR] + gainCenter*in[chC] + gainSurround*in[chRs]
			if a := abs32(in[chLFE]); a > lfePeak {
				lfePeak = a
			}
		}
		out[2*n], out[2*n+1] = l, r
	}
	return out, lfePeak, nil
}

func abs32(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}
