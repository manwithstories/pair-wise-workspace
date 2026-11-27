// Package normalize converts audio to 48 kHz stereo and validates
// per-track color metadata.
package normalize

import (
	"fmt"

	"mediad/internal/media"
)

// Layout names a confirmed channel layout.
type Layout string

const (
	LayoutMono   Layout = "mono"
	LayoutStereo Layout = "stereo"
	Layout51     Layout = "5.1"
)

// TargetRate is the unified output sample rate.
const TargetRate = int64(48000)

// KnownLayout reports whether layout is confirmed for channels. An
// unconfirmed layout must produce an error, never a guess.
func KnownLayout(layout Layout, channels int) bool {
	switch layout {
	case LayoutMono:
		return channels == 1
	case LayoutStereo:
		return channels == 2
	case Layout51:
		return channels == 6
	}
	return false
}

func clamp16(v float64) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	if v >= 0 {
		return int16(v + 0.5)
	}
	return int16(v - 0.5)
}

// Downmix maps interleaved samples to stereo using fixed coefficients.
// A missing or inconsistent layout is a fault, not a default.
func Downmix(src []int16, channels int, layout Layout) ([]int16, *media.Fault) {
	if !KnownLayout(layout, channels) {
		return nil, &media.Fault{
			Kind: media.FaultLayoutMissing, Op: "normalize.audio",
			Msg: fmt.Sprintf("no confirmed layout for %d channel(s) (layout %q)", channels, layout),
		}
	}
	frames := len(src) / channels
	out := make([]int16, frames*2)
	switch layout {
	case LayoutMono:
		for i := 0; i < frames; i++ {
			out[2*i], out[2*i+1] = src[i], src[i]
		}
	case LayoutStereo:
		copy(out, src[:frames*2])
	case Layout51:
		// L,R,C,LFE,Ls,Rs -> stereo; LFE is dropped.
		const c = 0.7071
		for i := 0; i < frames; i++ {
			b := src[i*6:]
			out[2*i] = clamp16(float64(b[0]) + c*float64(b[2]) + c*float64(b[4]))
			out[2*i+1] = clamp16(float64(b[1]) + c*float64(b[2]) + c*float64(b[5]))
		}
	}
	return out, nil
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// Resampler converts interleaved stereo PCM from one rate to another
// using exact rational fixed-point linear interpolation. The position
// is tracked as an integer in units of 1/(To/gcd) source frames, so
// chunk boundaries are sample-exact and never drift. At the trailing
// edge of a chunk the last frame is held constant (clamped) when an
// output instant falls between it and the not-yet-available next
// chunk; the position state is serializable for checkpoints.
type Resampler struct {
	From, To int64
	unit     int64 // To / gcd(From, To)
	step     int64 // From / gcd(From, To)
	pos      int64 // position in 1/unit source frames, in [0, step)
}

func NewResampler(from, to int64) *Resampler {
	if to <= 0 {
		to = TargetRate
	}
	g := gcd(from, to)
	return &Resampler{From: from, To: to, unit: to / g, step: from / g}
}

// State returns the serializable resampler position.
func (r *Resampler) State() int64 { return r.pos }

// SetState restores the resampler position, e.g. after a checkpoint
// load.
func (r *Resampler) SetState(pos int64) { r.pos = pos }

func lerp(a, b int16, frac uint64) int16 {
	return int16(int32(a) + int32((int64(b)-int64(a))*int64(frac)>>32))
}

// Process converts one chunk of interleaved stereo samples. The number
// of output frames follows the exact rate ratio; the fractional
// remainder stays in the position accumulator for the next chunk.
func (r *Resampler) Process(src []int16) []int16 {
	frames := int64(len(src) / 2)
	if frames == 0 {
		return nil
	}
	end := frames * r.unit
	out := make([]int16, 0, 2*(frames*r.To/r.From+2))
	for r.pos < end {
		idx := r.pos / r.unit
		frac := uint64((r.pos % r.unit) << 32 / r.unit)
		l0, r0 := src[2*idx], src[2*idx+1]
		l1, r1 := l0, r0
		if idx+1 < frames {
			l1, r1 = src[2*idx+2], src[2*idx+3]
		}
		out = append(out, lerp(l0, l1, frac), lerp(r0, r1, frac))
		r.pos += r.step
	}
	r.pos -= end
	return out
}
