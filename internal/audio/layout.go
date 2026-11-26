// Package audio validates channel mapping and sample-rate consistency.
package audio

import (
	"fmt"

	"mediad/internal/media"
)

// Layouts maps channel count to canonical labels used by the validator.
var Layouts = map[uint16][]string{
	1: {"FC"},
	2: {"L", "R"},
	3: {"L", "R", "C"},
	6: {"L", "R", "C", "LFE", "Ls", "Rs"},
	8: {"L", "R", "C", "LFE", "Ls", "Rs", "Lsr", "Rsr"},
}

// Report is the audio validation outcome.
type Report struct {
	TrackID uint32         `json:"track_id"`
	OK      bool           `json:"ok"`
	Repairs int            `json:"repairs"`
	Faults  []*media.Fault `json:"faults,omitempty"`
}

// Validate detects rate/channel conflicts. It repairs only when independent
// evidence (explicit labels, declared alternate value, or mdhd clock) supports
// the result; otherwise the track is marked failed rather than silently changed.
func Validate(t *media.Track) Report {
	rep := Report{TrackID: trackID(t), OK: true}
	if t == nil || t.Kind != media.KindAudio {
		return rep
	}
	if t.Audio == nil {
		rep.OK = false
		f := media.NewFault(media.CodeAudioLayout, "audio track lacks channel and sample-rate metadata").
			WithTrack(t.ID).WithBox("audt").Fatal().
			WithBasis("no channel map or rate evidence")
		rep.Faults = append(rep.Faults, f)
		t.Faults = append(t.Faults, f)
		return rep
	}
	a := t.Audio

	if a.SampleRate == 0 {
		rep.OK = false
		f := media.NewFault(media.CodeAudioRate, "sample rate is zero").WithTrack(t.ID).WithBox("audt").Fatal()
		rep.Faults = append(rep.Faults, f)
	} else if t.Timescale != 0 && a.SampleRate != t.Timescale {
		original := fmt.Sprintf("sample_rate=%d mdhd_timescale=%d", a.SampleRate, t.Timescale)
		switch {
		case a.DeclaredRate == t.Timescale && a.DeclaredRate != 0:
			f := media.NewFault(media.CodeAudioRate, "sample-rate conflict").
				WithTrack(t.ID).WithBox("audt").WithOriginal(original).
				WithBasis("declared rate matches mdhd timescale")
			a.SampleRate = a.DeclaredRate
			a.RepairBasis = f.RepairBasis
			rep.Repairs++
			rep.Faults = append(rep.Faults, f)
		default:
			rep.OK = false
			f := media.NewFault(media.CodeAudioRate, "sample rate %d conflicts with track clock %d and no corroborating declaration", a.SampleRate, t.Timescale).
				WithTrack(t.ID).WithBox("audt").WithOriginal(original).Fatal().
				WithBasis("ambiguous sample-rate evidence")
			rep.Faults = append(rep.Faults, f)
		}
	}

	if a.ChannelCount == 0 {
		rep.OK = false
		f := media.NewFault(media.CodeAudioLayout, "channel count is zero").WithTrack(t.ID).WithBox("audt").Fatal()
		rep.Faults = append(rep.Faults, f)
		t.Faults = append(t.Faults, f)
		return rep
	}
	if len(a.Layout) == 0 {
		if labels, ok := Layouts[a.ChannelCount]; ok {
			a.Layout = append([]string(nil), labels...)
			a.RepairBasis = "canonical layout for declared channel count"
			rep.Repairs++
		} else {
			rep.OK = false
			f := media.NewFault(media.CodeAudioLayout, "no canonical layout for %d channels", a.ChannelCount).
				WithTrack(t.ID).WithBox("audt").Fatal().WithBasis("unknown channel layout")
			rep.Faults = append(rep.Faults, f)
		}
	} else if len(a.Layout) != int(a.ChannelCount) {
		original := fmt.Sprintf("channel_count=%d layout_labels=%d declared_channels=%d", a.ChannelCount, len(a.Layout), a.DeclaredChannels)
		switch {
		case int(a.DeclaredChannels) == len(a.Layout) && knownLayout(len(a.Layout)):
			f := media.NewFault(media.CodeAudioLayout, "channel count conflicts with explicit layout labels").
				WithTrack(t.ID).WithBox("audt").WithOriginal(original).
				WithBasis("explicit channel labels and declared channel count agree")
			a.ChannelCount = a.DeclaredChannels
			rep.Repairs++
			rep.Faults = append(rep.Faults, f)
		default:
			rep.OK = false
			f := media.NewFault(media.CodeAudioLayout, "channel count %d does not match %d layout labels and cannot be inferred", a.ChannelCount, len(a.Layout)).
				WithTrack(t.ID).WithBox("audt").WithOriginal(original).Fatal().
				WithBasis("conflicting count and labels without corroboration")
			rep.Faults = append(rep.Faults, f)
		}
	}
	if int(a.ChannelCount) != len(a.Layout) || !knownLayout(len(a.Layout)) {
		// If labels are non-canonical but count matches, that is still usable;
		// only reject count/label mismatch here.
		if int(a.ChannelCount) != len(a.Layout) {
			rep.OK = false
		}
	}
	for _, f := range rep.Faults {
		t.Faults = append(t.Faults, f)
	}
	return rep
}

func knownLayout(channels int) bool {
	_, ok := Layouts[uint16(channels)]
	return ok
}

func trackID(t *media.Track) uint32 {
	if t == nil {
		return 0
	}
	return t.ID
}
