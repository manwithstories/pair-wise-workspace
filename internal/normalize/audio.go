// Package normalize unifies audio channel layouts, sample rates and colour
// metadata decisions without fabricating pixel or sample conversion.
package normalize

import (
	"fmt"

	"mediaroute/internal/clock"
)

// Layout is an audio channel layout.
type Layout int

const (
	Mono Layout = iota
	Stereo
	Surround51
)

// Channels returns the channel count of the layout.
func (l Layout) Channels() int {
	switch l {
	case Mono:
		return 1
	case Stereo:
		return 2
	case Surround51:
		return 6
	default:
		return 0
	}
}

func (l Layout) String() string {
	switch l {
	case Mono:
		return "mono"
	case Stereo:
		return "stereo"
	case Surround51:
		return "5.1"
	default:
		return "unknown"
	}
}

// LayoutFromChannels maps a channel count to a known layout.
func LayoutFromChannels(n int) (Layout, error) {
	switch n {
	case 1:
		return Mono, nil
	case 2:
		return Stereo, nil
	case 6:
		return Surround51, nil
	default:
		return 0, fmt.Errorf("normalize: unsupported channel count %d", n)
	}
}

// AudioTrack describes one audio essence track.
type AudioTrack struct {
	Rate   int
	Layout Layout
}

// AudioDecision records how a track maps onto the house format.
type AudioDecision struct {
	RateIn, RateOut     int
	LayoutIn, LayoutOut Layout
	ResampleNum         int // output ticks per input sample numerator note
	Note                string
}

// NormalizeAudio decides how src maps onto dst. It performs metadata-level
// planning only: sample-accurate timeline conversion goes through
// clock.SamplesToTicks; no audio samples are synthesised here.
func NormalizeAudio(dst, src AudioTrack) (AudioDecision, error) {
	if src.Rate != clock.Rate44100 && src.Rate != clock.Rate48000 {
		return AudioDecision{}, fmt.Errorf("normalize: unsupported rate %d", src.Rate)
	}
	d := AudioDecision{
		RateIn: src.Rate, RateOut: dst.Rate,
		LayoutIn: src.Layout, LayoutOut: dst.Layout,
	}
	switch {
	case src.Rate == dst.Rate && src.Layout == dst.Layout:
		d.Note = "passthrough"
	case src.Rate != dst.Rate && src.Layout == dst.Layout:
		d.Note = fmt.Sprintf("resample %d->%d by sample count", src.Rate, dst.Rate)
	case src.Rate == dst.Rate:
		d.Note = fmt.Sprintf("remap %s->%s", src.Layout, dst.Layout)
	default:
		d.Note = fmt.Sprintf("resample %d->%d and remap %s->%s", src.Rate, dst.Rate, src.Layout, dst.Layout)
	}
	return d, nil
}
