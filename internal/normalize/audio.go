// Package normalize turns the heterogeneous per-camera descriptions into
// explicit, publishable groups. It never resamples or re-encodes: an
// audio track keeps its sample rate and layout, and a layout that cannot
// be represented is rejected rather than silently folded into stereo.
package normalize

import (
	"fmt"

	"fastpost/internal/model"
)

// ChannelLayout names the channel configurations the remuxer can publish.
type ChannelLayout string

const (
	LayoutMono       ChannelLayout = "mono"
	LayoutStereo     ChannelLayout = "stereo"
	LayoutSurround51 ChannelLayout = "5.1"
)

// channelCount maps a layout to the number of channels it must carry.
var channelCount = map[ChannelLayout]int{
	LayoutMono:       1,
	LayoutStereo:     2,
	LayoutSurround51: 6,
}

// AudioDesc describes one pre-encoded audio track.
type AudioDesc struct {
	Track      string
	Channels   int
	SampleRate int
	// Codec is informational only; the remuxer never transcodes.
	Codec string
}

// AudioGroup is a publishable EXT-X-MEDIA rendition group. Tracks in the
// same group share a layout and sample rate, which keeps every rendition
// internally consistent without any resampling.
type AudioGroup struct {
	GroupID    string
	Layout     ChannelLayout
	SampleRate int
	Tracks     []AudioDesc
}

// GroupAudio partitions tracks into explicit groups keyed by layout and
// sample rate. A track whose channel count does not match a known layout,
// or whose sample rate is unsupported, fails with a structured fault; the
// caller must not publish a partial result.
func GroupAudio(tracks []AudioDesc) ([]AudioGroup, error) {
	col := &model.Collector{}
	groups := map[string]*AudioGroup{}
	var order []string
	for _, tr := range tracks {
		layout, ok := layoutForChannels(tr.Channels)
		if !ok {
			col.Add(model.FaultChannelMapping, tr.Track, -1,
				fmt.Sprintf("illegal channel count %d: no publishable layout", tr.Channels))
			continue
		}
		if !supportedRate(tr.SampleRate) {
			col.Add(model.FaultChannelMapping, tr.Track, -1,
				fmt.Sprintf("unsupported sample rate %d Hz", tr.SampleRate))
			continue
		}
		id := groupID(layout, tr.SampleRate)
		g, ok := groups[id]
		if !ok {
			g = &AudioGroup{GroupID: id, Layout: layout, SampleRate: tr.SampleRate}
			groups[id] = g
			order = append(order, id)
		}
		g.Tracks = append(g.Tracks, tr)
	}
	if col.Has() {
		return nil, col.Err()
	}
	out := make([]AudioGroup, 0, len(order))
	for _, id := range order {
		out = append(out, *groups[id])
	}
	return out, nil
}

// layoutForChannels resolves a channel count to a named layout. Counts
// outside the published set return false so the caller can reject them.
func layoutForChannels(n int) (ChannelLayout, bool) {
	switch n {
	case 1:
		return LayoutMono, true
	case 2:
		return LayoutStereo, true
	case 6:
		return LayoutSurround51, true
	default:
		return "", false
	}
}

// supportedRate reports whether a sample rate is one the remuxer is
// allowed to pass through. Both 44.1 kHz and 48 kHz are kept as distinct
// groups; nothing is resampled to the other.
func supportedRate(rate int) bool {
	switch rate {
	case 44100, 48000:
		return true
	default:
		return false
	}
}

func groupID(layout ChannelLayout, rate int) string {
	return "audio-" + string(layout) + "-" + fmt.Sprintf("%d", rate)
}

// Rendition renders an HLS CHANNELS attribute value for the group.
func (g AudioGroup) RenditionChannels() int {
	return channelCount[g.Layout]
}
