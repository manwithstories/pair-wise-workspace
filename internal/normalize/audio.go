// Package normalize turns vendor track differences (channel layout,
// sample rate, color metadata) into explicit, auditable decisions.
// Nothing is silently dropped: every deviation is recorded as a note or
// rejected with a fault, per policy.
package normalize

import (
	"fmt"

	"aircheck/internal/fault"
	"aircheck/internal/scan"
)

// knownLayouts maps channel layouts we can normalize with confidence.
var knownLayouts = map[string]int{
	"mono": 1, "stereo": 2, "2.1": 3,
	"5.1": 6, "5.1(side)": 6, "7.1": 8,
}

// AudioPolicy declares how audio deviations are handled.
type AudioPolicy struct {
	TargetRate     int    `json:"target_rate"`      // e.g. 48000
	TargetChannels int    `json:"target_channels"`  // e.g. 2
	OnUnknownLayout string `json:"on_unknown_layout"` // "reject" or "downmix"
}

// DefaultAudioPolicy is the broadcast-safe default.
func DefaultAudioPolicy() AudioPolicy {
	return AudioPolicy{TargetRate: 48000, TargetChannels: 2, OnUnknownLayout: "reject"}
}

// AudioDecision is the per-track normalization plan.
type AudioDecision struct {
	Resample     bool     `json:"resample"`
	Downmix      bool     `json:"downmix"`
	TargetRate   int      `json:"target_rate"`
	TargetCh     int      `json:"target_channels"`
	Notes        []string `json:"notes,omitempty"`
}

// PlanAudio decides how one audio track is normalized. Unknown channel
// layouts fault with CodeChannelLayout unless policy downgrades.
func PlanAudio(t scan.Track, p AudioPolicy) (AudioDecision, error) {
	d := AudioDecision{TargetRate: p.TargetRate, TargetCh: p.TargetChannels}
	if t.Kind != "audio" {
		return d, nil
	}
	layout := t.ChannelLayout
	if layout == "" {
		layout = layoutFromChannels(t.Channels)
	}
	if _, ok := knownLayouts[layout]; !ok {
		err := fault.New(fault.CodeChannelLayout, t.ID, -1, p.OnUnknownLayout == "downmix",
			"unknown channel layout %q (%d ch)", t.ChannelLayout, t.Channels)
		if p.OnUnknownLayout != "downmix" {
			return d, err
		}
		d.Notes = append(d.Notes, err.Error())
		d.Downmix = true
	} else if knownLayouts[layout] != p.TargetChannels {
		d.Downmix = true
		d.Notes = append(d.Notes, fmt.Sprintf("downmix %s -> %dch", layout, p.TargetChannels))
	}
	if t.SampleRate != 0 && t.SampleRate != p.TargetRate {
		d.Resample = true
		d.Notes = append(d.Notes, fmt.Sprintf("resample %d -> %d Hz", t.SampleRate, p.TargetRate))
	}
	return d, nil
}

func layoutFromChannels(ch int) string {
	for name, n := range knownLayouts {
		if n == ch {
			return name
		}
	}
	return ""
}

// Args renders the ffmpeg audio filter/output flags for a decision.
func (d AudioDecision) Args() []string {
	var args []string
	if d.Resample {
		args = append(args, "-ar", fmt.Sprint(d.TargetRate))
	}
	if d.Downmix {
		args = append(args, "-ac", fmt.Sprint(d.TargetCh))
	}
	return args
}
