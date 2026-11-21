// Package audio normalizes channel-layout and sample-rate declarations while
// keeping the raw values. It never resamples or folds channels: plans that
// would require an implicit transform are rejected until an explicit retain
// policy is supplied.
package audio

import (
	"fmt"
	"sort"

	"mkvarchive/internal/model"
)

// RetentionPolicy states how the plan is allowed to treat audio semantics.
type RetentionPolicy int

const (
	// PolicyReject refuses any plan whose tracks do not share one sample
	// rate and whose layouts need implicit adaptation.
	PolicyReject RetentionPolicy = iota
	// PolicyRetain explicitly accepts verbatim retention of every per-track
	// declaration; conflicts become manifest warnings, never silent fixes.
	PolicyRetain
)

// Layout is the canonical label for a channel-count declaration.
type Layout string

const (
	LayoutMono       Layout = "mono"
	LayoutStereo     Layout = "stereo"
	LayoutSurround51 Layout = "5.1"
	LayoutSurround71 Layout = "7.1"
	LayoutUnknown    Layout = "unknown"
)

// NormalizeLayout maps a declared channel count to a canonical layout label.
// Both the raw count and the label are kept so reviewers can audit the rule.
func NormalizeLayout(channels uint64) Layout {
	switch channels {
	case 1:
		return LayoutMono
	case 2:
		return LayoutStereo
	case 6:
		return LayoutSurround51
	case 8:
		return LayoutSurround71
	default:
		return LayoutUnknown
	}
}

// LayoutReport is one track's raw declaration plus its normalized label.
type LayoutReport struct {
	SourceTrackUID    uint64  `json:"source_track_uid"`
	DeclaredChannels  uint64  `json:"declared_channels"`
	NormalizedLayout  Layout  `json:"normalized_layout"`
	DeclaredLayoutRaw string  `json:"declared_layout_raw,omitempty"`
	SampleRateHz      float64 `json:"sample_rate_hz"`
}

// ValidateTracks audits all audio tracks against the retention policy.
// With PolicyReject, a non-uniform sample rate or an unknown layout is a
// hard error. With PolicyRetain, every conflict is returned as a warning
// carrying the per-track values; nothing is ever unified silently.
func ValidateTracks(tracks []*model.Track, policy RetentionPolicy) ([]LayoutReport, []*model.ArchiveError, error) {
	reports := make([]LayoutReport, 0, len(tracks))
	var warnings []*model.ArchiveError
	rates := map[float64][]uint64{}
	for _, tr := range tracks {
		if tr.Kind != model.KindAudio || tr.Audio == nil {
			continue
		}
		a := tr.Audio
		report := LayoutReport{
			SourceTrackUID:    tr.UID,
			DeclaredChannels:  a.Channels,
			NormalizedLayout:  NormalizeLayout(a.Channels),
			DeclaredLayoutRaw: a.PositionsDeclaredRaw,
			SampleRateHz:      a.SamplingFrequency,
		}
		reports = append(reports, report)
		rates[a.SamplingFrequency] = append(rates[a.SamplingFrequency], tr.UID)

		if report.NormalizedLayout == LayoutUnknown {
			w := model.NewWarning("AUDIO_UNKNOWN_LAYOUT",
				fmt.Sprintf("track %d declares %d channels; layout retained as %s",
					tr.UID, a.Channels, LayoutUnknown), tr.HeaderOffset)
			if policy == PolicyReject {
				return nil, nil, w
			}
			warnings = append(warnings, w)
		}
		if a.HasChannelPositions && !layoutMatchesChannels(a.ChannelPositions, a.Channels) {
			w := model.NewWarning("AUDIO_CHANNEL_MISMATCH",
				fmt.Sprintf("track %d ChannelPositions does not match %d declared channels",
					tr.UID, a.Channels), tr.HeaderOffset)
			if policy == PolicyReject {
				return nil, nil, w
			}
			warnings = append(warnings, w)
		}
	}
	if len(rates) > 1 {
		msg := "sample rates differ across audio tracks; per-track values retained: "
		msg += rateSummary(rates)
		w := model.NewWarning("AUDIO_SAMPLE_RATE_CONFLICT", msg, -1)
		if policy == PolicyReject {
			return nil, nil, w
		}
		warnings = append(warnings, w)
	}
	return reports, warnings, nil
}

// rateSummary renders each distinct rate with the tracks using it, sorted
// for deterministic output.
func rateSummary(rates map[float64][]uint64) string {
	values := make([]float64, 0, len(rates))
	for rate := range rates {
		values = append(values, rate)
	}
	sort.Float64s(values)
	out := ""
	for i, rate := range values {
		if i > 0 {
			out += "; "
		}
		out += fmt.Sprintf("%.0fHz->tracks%v", rate, rates[rate])
	}
	return out
}

// layoutMatchesChannels validates the Matroska ChannelPositions table: one
// 32-bit horizontal angle per declared channel, so the payload length must
// be exactly 4*channels.
func layoutMatchesChannels(positions []byte, channels uint64) bool {
	return uint64(len(positions)) == channels*4
}
