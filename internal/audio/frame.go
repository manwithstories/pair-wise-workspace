// Package audio defines the unified sample stream description: channel
// layout, per-channel loudness weights and stream start time. All samples
// inside the pipeline are interleaved float64 normalized to [-1.0, 1.0].
package audio

import "fmt"

// Layout identifies a channel ordering convention.
type Layout int

const (
	LayoutAuto Layout = iota // derive from channel count
	LayoutMono
	LayoutStereo
	LayoutQuad
	Layout51
	Layout71
	LayoutGeneric // unknown channel count, all weights 1.0
)

// ChannelNames is the canonical order for multichannel layouts:
// FL, FR, FC, LFE, BL, BR, SL, SR.
var ChannelNames = []string{"FL", "FR", "FC", "LFE", "BL", "BR", "SL", "SR"}

// MaxChannels is the largest channel count the pipeline accepts.
const MaxChannels = 8

// StreamInfo describes a decoded sample stream.
type StreamInfo struct {
	SampleRate int
	Channels   int
	Layout     Layout
	StartFrame int64 // first sample frame index (non-zero for BWF time refs)
}

// ParseLayout parses "auto", "mono"/"1.0", "stereo"/"2.0", "4.0", "5.1", "7.1".
func ParseLayout(s string) (Layout, error) {
	switch s {
	case "", "auto":
		return LayoutAuto, nil
	case "mono", "1.0":
		return LayoutMono, nil
	case "stereo", "2.0":
		return LayoutStereo, nil
	case "4.0", "quad":
		return LayoutQuad, nil
	case "5.1":
		return Layout51, nil
	case "7.1":
		return Layout71, nil
	}
	return LayoutAuto, fmt.Errorf("unknown layout %q (want auto|mono|stereo|4.0|5.1|7.1)", s)
}

// Channels returns the channel count implied by the layout (0 for Auto/Generic).
func (l Layout) Channels() int {
	switch l {
	case LayoutMono:
		return 1
	case LayoutStereo:
		return 2
	case LayoutQuad:
		return 4
	case Layout51:
		return 6
	case Layout71:
		return 8
	}
	return 0
}

func (l Layout) String() string {
	switch l {
	case LayoutAuto:
		return "auto"
	case LayoutMono:
		return "mono"
	case LayoutStereo:
		return "stereo"
	case LayoutQuad:
		return "4.0"
	case Layout51:
		return "5.1"
	case Layout71:
		return "7.1"
	}
	return "generic"
}

// LayoutForChannels picks the default layout for a channel count.
func LayoutForChannels(n int) Layout {
	switch n {
	case 1:
		return LayoutMono
	case 2:
		return LayoutStereo
	case 4:
		return LayoutQuad
	case 6:
		return Layout51
	case 8:
		return Layout71
	}
	return LayoutGeneric
}

// ResolveLayout combines a declared layout with the actual channel count.
// A declared layout whose channel count mismatches the stream is an error.
func ResolveLayout(declared Layout, channels int) (Layout, error) {
	if declared == LayoutAuto {
		return LayoutForChannels(channels), nil
	}
	if declared.Channels() != channels {
		return LayoutAuto, fmt.Errorf("declared layout %s (%d ch) does not match stream (%d ch)",
			declared, declared.Channels(), channels)
	}
	return declared, nil
}

// Weights returns the BS.1770 channel weights for a layout applied to a
// stream with the given channel count. lfe reports whether an LFE channel
// is present; its weight is always 0 (excluded from loudness per spec).
func Weights(l Layout, channels int) (w []float64, lfe bool) {
	w = make([]float64, channels)
	for i := range w {
		w[i] = 1.0
	}
	switch l {
	case Layout51:
		if channels == 6 {
			w[3] = 0 // LFE
			lfe = true
		}
	case Layout71:
		if channels == 8 {
			w[3] = 0 // LFE
			lfe = true
		}
	}
	return w, lfe
}
