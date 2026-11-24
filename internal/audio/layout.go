package audio

import (
	"math"

	"mediarig/internal/media"
)

// Layout is the explicit source channel-layout enumeration.
type Layout string

const (
	LayoutMono     Layout = "mono"
	LayoutStereo   Layout = "stereo"
	LayoutFiveOne  Layout = "5.1"
	LayoutSevenOne Layout = "7.1"
)

// SupportedLayouts lists every accepted input layout. All convert to stereo.
func SupportedLayouts() []Layout {
	return []Layout{LayoutMono, LayoutStereo, LayoutFiveOne, LayoutSevenOne}
}

func ParseLayout(s string) (Layout, error) {
	switch Layout(s) {
	case LayoutMono:
		return LayoutMono, nil
	case LayoutStereo:
		return LayoutStereo, nil
	case LayoutFiveOne:
		return LayoutFiveOne, nil
	case LayoutSevenOne:
		return LayoutSevenOne, nil
	default:
		return "", media.Policy(media.StageAudio, "unsupported channel layout %q", s)
	}
}

// Channels returns the source channel count and defined channel order.
//
// 5.1 order:  L, R, C, LFE, Ls, Rs
// 7.1 order:  L, R, C, LFE, Lss, Rss, Lrs, Rrs
func (l Layout) Channels() int {
	switch l {
	case LayoutMono:
		return 1
	case LayoutStereo:
		return 2
	case LayoutFiveOne:
		return 6
	case LayoutSevenOne:
		return 8
	default:
		return 0
	}
}

func (l Layout) ChannelNames() []string {
	switch l {
	case LayoutMono:
		return []string{"M"}
	case LayoutStereo:
		return []string{"L", "R"}
	case LayoutFiveOne:
		return []string{"L", "R", "C", "LFE", "Ls", "Rs"}
	default:
		return []string{"L", "R", "C", "LFE", "Lss", "Rss", "Lrs", "Rrs"}
	}
}

// Explicit downmix coefficients. Center uses the standard -3 dB pan
// coefficient; LFE is mixed at -6 dB to each output. Surround channels use
// the same -3 dB coefficient.
const (
	CenterMixCoefficient   = 1 / math.Sqrt2
	LFEMixCoefficient      = 0.5
	SurroundMixCoefficient = 1 / math.Sqrt2
)

// SampleFormat enumerates supported RTP L16/normalized PCM representations.
type SampleFormat string

const (
	SampleFormatL16BE SampleFormat = "L16"
	SampleFormatS16LE SampleFormat = "s16le"
	SampleFormatF32   SampleFormat = "f32"
)

func SupportedSampleFormats() []SampleFormat {
	return []SampleFormat{SampleFormatL16BE, SampleFormatS16LE, SampleFormatF32}
}

// SupportedRates is the explicit sample-rate enumeration accepted by the rig.
var SupportedRates = []int{8000, 16000, 24000, 44100, 48000}

func IsSupportedRate(rate int) bool {
	for _, r := range SupportedRates {
		if r == rate {
			return true
		}
	}
	return false
}

// Audio is channel-major normalized floating PCM. The float representation is
// an internal DSP carrier; timeline lengths remain integer frame counts.
type Audio struct {
	SampleRate int
	Layout     Layout
	Channels   [][]float64
}

func NewAudio(rate int, channels int, frames int) Audio {
	ch := make([][]float64, channels)
	for i := range ch {
		ch[i] = make([]float64, frames)
	}
	return Audio{SampleRate: rate, Channels: ch}
}

func (a Audio) Frames() int {
	if len(a.Channels) == 0 {
		return 0
	}
	return len(a.Channels[0])
}

// DecodeL16BE decodes RFC-style signed 16-bit big-endian interleaved samples.
func DecodeL16BE(payload []byte, channels int) (Audio, error) {
	if channels <= 0 || len(payload)%(channels*2) != 0 {
		return Audio{}, media.Corrupt(media.StageAudio, "invalid L16 payload alignment for %d channels", channels)
	}
	frames := len(payload) / (channels * 2)
	out := NewAudio(0, channels, frames)
	for i := 0; i < frames; i++ {
		for c := 0; c < channels; c++ {
			o := (i*channels + c) * 2
			v := int16(uint16(payload[o])<<8 | uint16(payload[o+1]))
			out.Channels[c][i] = float64(v) / 32768.0
		}
	}
	return out, nil
}

// EncodeS16LE encodes stereo normalized PCM as signed 16-bit little-endian.
// Out-of-range values are clamped; callers report clipping separately.
func EncodeS16LE(a Audio) []byte {
	frames := a.Frames()
	buf := make([]byte, frames*2*2)
	for i := 0; i < frames; i++ {
		for c := 0; c < 2; c++ {
			v := a.Channels[c][i]
			if v > 1 {
				v = 1
			} else if v < -1 {
				v = -1
			}
			s := int16(math.Round(v * 32767))
			o := (i*2 + c) * 2
			buf[o] = byte(s)
			buf[o+1] = byte(s >> 8)
		}
	}
	return buf
}

// Downmix converts any supported layout to stereo using explicit coefficients.
func Downmix(in Audio, layout Layout) (Audio, error) {
	n := layout.Channels()
	if n == 0 || len(in.Channels) != n {
		return Audio{}, media.Policy(media.StageAudio, "layout %q requires %d channels, got %d", layout, n, len(in.Channels))
	}
	frames := in.Frames()
	out := NewAudio(in.SampleRate, 2, frames)
	L, R := out.Channels[0], out.Channels[1]

	switch layout {
	case LayoutMono:
		for i := 0; i < frames; i++ {
			v := in.Channels[0][i] * CenterMixCoefficient
			L[i], R[i] = v, v
		}
	case LayoutStereo:
		copy(L, in.Channels[0])
		copy(R, in.Channels[1])
	case LayoutFiveOne:
		// 0 L, 1 R, 2 C, 3 LFE, 4 Ls, 5 Rs
		for i := 0; i < frames; i++ {
			c := in.Channels[2][i] * CenterMixCoefficient
			lfe := in.Channels[3][i] * LFEMixCoefficient
			ls := in.Channels[4][i] * SurroundMixCoefficient
			rs := in.Channels[5][i] * SurroundMixCoefficient
			L[i] = in.Channels[0][i] + c + lfe + ls
			R[i] = in.Channels[1][i] + c + lfe + rs
		}
	case LayoutSevenOne:
		// 0 L,1 R,2 C,3 LFE,4 Lss,5 Rss,6 Lrs,7 Rrs
		for i := 0; i < frames; i++ {
			c := in.Channels[2][i] * CenterMixCoefficient
			lfe := in.Channels[3][i] * LFEMixCoefficient
			lSide := in.Channels[4][i] * SurroundMixCoefficient
			rSide := in.Channels[5][i] * SurroundMixCoefficient
			lRear := in.Channels[6][i] * SurroundMixCoefficient * 0.5
			rRear := in.Channels[7][i] * SurroundMixCoefficient * 0.5
			L[i] = in.Channels[0][i] + c + lfe + lSide + lRear
			R[i] = in.Channels[1][i] + c + lfe + rSide + rRear
		}
	default:
		return Audio{}, media.Policy(media.StageAudio, "unknown layout")
	}
	out.Layout = LayoutStereo
	return out, nil
}
