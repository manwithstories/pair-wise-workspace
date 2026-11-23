// Package normalize produces unified track descriptions from divergent
// source tracks: channel layouts, sample rates and colour metadata.
// Unsupported configurations block downstream nodes explicitly — they are
// never reported as success.
package normalize

import (
	"fmt"

	"archivecheck/internal/fault"
)

// Layouts maps channel count to a named layout.
var Layouts = map[int]string{
	1: "mono",
	2: "stereo",
	6: "5.1",
	8: "7.1",
}

// SupportedRates is the set of sample rates the archive accepts.
var SupportedRates = map[int]bool{
	8000: true, 16000: true, 22050: true, 24000: true,
	32000: true, 44100: true, 48000: true, 96000: true,
}

// AudioDesc is the normalized description of one audio track.
type AudioDesc struct {
	Track      int    `json:"track"`
	Channels   int    `json:"channels"`
	Layout     string `json:"layout"`
	SampleRate int    `json:"sample_rate"`
}

// Audio validates one audio track. An unsupported layout or sample rate
// yields a blocking fault whose chain starts at the normalize node.
func Audio(track, channels, sampleRate int) (*AudioDesc, *fault.Fault) {
	layout, ok := Layouts[channels]
	if !ok {
		return nil, &fault.Fault{
			Kind: fault.KindUnsupportedLayout, Track: track, Sample: -1, Offset: -1,
			Chain:       []string{"normalize", "segment", "checksum"},
			Recoverable: false,
			Message:     fmt.Sprintf("unsupported channel layout: %d channels", channels),
		}
	}
	if !SupportedRates[sampleRate] {
		return nil, &fault.Fault{
			Kind: fault.KindUnsupportedLayout, Track: track, Sample: -1, Offset: -1,
			Chain:       []string{"normalize", "segment", "checksum"},
			Recoverable: false,
			Message:     fmt.Sprintf("unsupported sample rate: %d Hz", sampleRate),
		}
	}
	return &AudioDesc{Track: track, Channels: channels, Layout: layout, SampleRate: sampleRate}, nil
}
