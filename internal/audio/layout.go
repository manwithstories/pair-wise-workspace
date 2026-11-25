package audio

import "avpipe/internal/media"

// Downmixer converts any supported channel layout to interleaved stereo.
type Downmixer struct{}

// NewDownmixer constructs a downmixer.
func NewDownmixer() *Downmixer { return &Downmixer{} }

// Process maps one interleaved PCM block to stereo. Mono is duplicated;
// 5.1 uses the standard film downmix coefficients. Output is always a whole
// number of stereo frames.
func (d *Downmixer) Process(in []int16, layout media.ChannelLayout) []int16 {
	switch layout {
	case media.LayoutStereo:
		out := make([]int16, len(in))
		copy(out, in)
		return out
	case media.LayoutMono:
		out := make([]int16, len(in)*2)
		for i, s := range in {
			out[2*i] = s
			out[2*i+1] = s
		}
		return out
	case media.LayoutFiveOne:
		return downmixFiveOne(in)
	default:
		return nil
	}
}

// 5.1 channel order: L, R, C, LFE, Ls, Rs.
// Film downmix (per ITU-R BS.775 style), scaled to avoid clipping:
//
//	Lt = L + (-3 dB)*C + (-3 dB)*Ls
//	Rt = R + (-3 dB)*C + (-3dB)*Rs
//
// LFE is dropped in the stereo fold-down.
func downmixFiveOne(in []int16) []int16 {
	const (
		centerGain = 0.70710678 // -3 dB
		surround   = 0.70710678 // -3 dB
	)
	frames := len(in) / media.LayoutFiveOne.Channels()
	out := make([]int16, frames*2)
	for f := 0; f < frames; f++ {
		base := f * 6
		l := float64(in[base+0])
		r := float64(in[base+1])
		c := float64(in[base+2])
		ls := float64(in[base+4])
		rs := float64(in[base+5])
		lt := l + c*centerGain + ls*surround
		rt := r + c*centerGain + rs*surround
		out[2*f] = clampInt16(lt)
		out[2*f+1] = clampInt16(rt)
	}
	return out
}
