// Package audio normalizes decoded audio tracks onto one canonical channel
// layout and sample rate. Every transformation requires an explicit mapping
// or resampling rule; unknown layouts and unsupported rate pairs block the
// downstream timeline instead of being guessed.
package audio

import (
	"fmt"

	"review/internal/media"
	"review/internal/mkv"
)

// Canonical layout and rate every normalized track converges to.
const (
	CanonicalChannels   uint32 = 2
	CanonicalSampleRate        = 48000.0
)

// Channel names in canonical stereo order.
const (
	ChLeft  = "L"
	ChRight = "R"
	ChCenter = "C"
	ChLFE    = "LFE"
	ChBL     = "BL"
	ChBR     = "BR"
)

// ChannelMap is an explicit source-to-canonical mapping keyed by source
// channel index (0-based). An unmapped source channel makes the plan invalid.
type ChannelMap map[int]string

// Plan describes how one audio track is normalized.
type Plan struct {
	Track          uint32
	InputChannels  uint32
	InputRate      float64
	ChannelMap     ChannelMap
	ResampleRatio  float64 // output samples per input sample
	BlockedBy      string  // first blocking dependency when unmappable
	BlockReason    string
}

// Mappable reports whether the plan can run.
func (p Plan) Mappable() bool { return p.BlockedBy == "" && p.BlockReason == "" }

// SupportedRates lists rates with an exact rational resampling factor.
// Any rate outside this table (or a zero/unknown rate) blocks the track.
var SupportedRates = map[float64]float64{
	8000:  CanonicalSampleRate / 8000,
	16000: CanonicalSampleRate / 16000,
	24000: CanonicalSampleRate / 24000,
	44100: CanonicalSampleRate / 44100,
	48000: 1,
}

// KnownLayouts maps channel counts to explicit default layouts. Counts
// outside this table are "unknown" and require a caller-supplied map.
var KnownLayouts = map[uint32]ChannelMap{
	1: {0: ChCenter},
	2: {0: ChLeft, 1: ChRight},
	6: {0: ChLeft, 1: ChRight, 2: ChCenter, 3: ChLFE, 4: ChBL, 5: ChBR},
}

// Result is the normalized view of a track; payloads are represented by
// deterministic frame counts rather than PCM bytes in this offline tool.
type Result struct {
	Track           uint32
	OutputChannels  uint32
	OutputRate      float64
	InputFrames     int
	OutputFrames    int
	Plan            Plan
}

// BuildPlan validates one track against an optional explicit channel map.
// A nil explicit map is accepted only for known channel counts.
func BuildPlan(tr mkv.Track, explicit ChannelMap) (Plan, error) {
	plan := Plan{
		Track:         tr.Number,
		InputChannels: tr.Channels,
		InputRate:     tr.SampleRate,
	}
	// Rule 1: channel layout must be explicitly resolvable.
	switch {
	case tr.Channels == 0:
		plan.BlockedBy = "mkv-decode"
		plan.BlockReason = "channel layout absent/unknown; supply an explicit map"
		return plan, media.NewConflict("audio-normalize",
			plan.BlockReason, plan.BlockedBy)
	case explicit != nil:
		if err := validateExplicit(tr.Channels, explicit); err != nil {
			plan.BlockedBy = "timeline-merge"
			plan.BlockReason = err.Error()
			return plan, media.NewConflict("audio-normalize",
				plan.BlockReason, plan.BlockedBy)
		}
		plan.ChannelMap = explicit
	default:
		layout, ok := KnownLayouts[tr.Channels]
		if !ok {
			plan.BlockedBy = "timeline-merge"
			plan.BlockReason = fmt.Sprintf(
				"no layout for %d channels; explicit mapping required", tr.Channels)
			return plan, media.NewConflict("audio-normalize",
				plan.BlockReason, plan.BlockedBy)
		}
		plan.ChannelMap = layout
	}
	// Rule 2: sample rate must have a known resampling factor.
	ratio, ok := SupportedRates[tr.SampleRate]
	if !ok || tr.SampleRate <= 0 {
		plan.BlockedBy = "timeline-merge"
		plan.BlockReason = fmt.Sprintf(
			"sample rate %.0f has no resampling rule", tr.SampleRate)
		return plan, media.NewConflict("audio-normalize",
			plan.BlockReason, plan.BlockedBy)
	}
	plan.ResampleRatio = ratio
	return plan, nil
}

func validateExplicit(channels uint32, m ChannelMap) error {
	seen := map[string]bool{}
	for i := uint32(0); i < channels; i++ {
		target, ok := m[int(i)]
		if !ok {
			return fmt.Errorf("explicit map leaves source channel %d unmapped", i)
		}
		if !isCanonicalChannel(target) {
			return fmt.Errorf("explicit map target %q is not canonical", target)
		}
		if seen[target] {
			return fmt.Errorf("explicit map sends two sources to %q", target)
		}
		seen[target] = true
	}
	return nil
}

func isCanonicalChannel(name string) bool {
	switch name {
	case ChLeft, ChRight, ChCenter, ChLFE, ChBL, ChBR:
		return true
	}
	return false
}

// Normalize applies a valid plan to a decoded block count. The output frame
// count is rounded deterministically (half-up) so repeated runs match.
func Normalize(plan Plan, inputFrames int) (Result, error) {
	if !plan.Mappable() {
		return Result{}, media.NewConflict("audio-normalize",
			"cannot normalize with an invalid plan", plan.BlockedBy)
	}
	out := int(float64(inputFrames)*plan.ResampleRatio + 0.5)
	return Result{
		Track:          plan.Track,
		OutputChannels: CanonicalChannels,
		OutputRate:     CanonicalSampleRate,
		InputFrames:    inputFrames,
		OutputFrames:   out,
		Plan:           plan,
	}, nil
}
