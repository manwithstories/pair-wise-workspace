// Package audio validates multi-channel layouts and sample rates and applies
// only explicitly permitted normalization. Unknown layouts, invalid sample
// rates and non-divisible time boundaries are fatal: the pipeline refuses to
// invent a conversion it cannot justify.
package audio

import (
	"fmt"

	"github.com/mediamend/mediamend/internal/model"
)

// Layout is a named speaker layout with its channel count and ordered map.
type Layout struct {
	Name     string
	Channels uint16
	Map      []string // channel labels in track order
}

var knownLayouts = map[string]Layout{
	"mono":   {"mono", 1, []string{"FC"}},
	"stereo": {"stereo", 2, []string{"FL", "FR"}},
	"5.1":    {"5.1", 6, []string{"FL", "FR", "FC", "LFE", "BL", "BR"}},
	"7.1":    {"7.1", 8, []string{"FL", "FR", "FC", "LFE", "BL", "BR", "SL", "SR"}},
}

// validRates are the sample rates mediamend accepts. Anything else is an
// unambiguous device misconfiguration, not a guessable normalization.
var validRates = map[uint32]bool{
	8000: true, 16000: true, 24000: true, 32000: true,
	44100: true, 48000: true, 96000: true,
}

// Spec describes one audio track's physical format.
type Spec struct {
	TrackID    string
	Layout     string
	SampleRate uint32
	// FrameSize is the number of samples carried by one audio frame.
	FrameSize uint32
	// FrameCount is the number of audio frames in the track.
	FrameCount uint64
}

// Policy is the explicit, caller-supplied normalization permission. No field
// here is inferred: a conversion whose flag is false is rejected rather than
// guessed.
type Policy struct {
	TargetLayout  string
	TargetRate    uint32
	AllowDownmix  bool // e.g. 5.1 -> stereo
	AllowUpmix    bool // e.g. mono -> stereo
	AllowResample bool // rate change to TargetRate
}

// LookupLayout returns a known layout and ok=false for an unknown name.
func LookupLayout(name string) (Layout, bool) {
	l, ok := knownLayouts[name]
	return l, ok
}

// Normalize validates spec and, when it differs from the policy target,
// applies an explicitly permitted conversion. Every applied conversion is
// returned as an info-level fault carrying the reason; every disallowed or
// impossible conversion is fatal.
func Normalize(spec Spec, pol Policy, file string) (Spec, []model.Fault) {
	out := spec
	var faults []model.Fault
	loc := model.SourceLocation{File: file, Track: spec.TrackID}

	layout, ok := knownLayouts[spec.Layout]
	if !ok {
		return out, append(faults, model.Fault{
			Class: model.FaultAudioLayout, Severity: model.SeverityFatal,
			Location: loc, Message: fmt.Sprintf("unknown channel layout %q", spec.Layout),
			Actual: spec.Layout,
		})
	}
	if !validRates[spec.SampleRate] {
		return out, append(faults, model.Fault{
			Class: model.FaultAudioLayout, Severity: model.SeverityFatal,
			Location: loc, Message: fmt.Sprintf("invalid sample rate %d", spec.SampleRate),
			Actual: fmt.Sprintf("%d", spec.SampleRate),
		})
	}

	// Layout normalization.
	if pol.TargetLayout != "" && spec.Layout != pol.TargetLayout {
		target, tok := knownLayouts[pol.TargetLayout]
		if !tok {
			return out, append(faults, model.Fault{
				Class: model.FaultAudioLayout, Severity: model.SeverityFatal,
				Location: loc, Message: fmt.Sprintf("policy targets unknown layout %q", pol.TargetLayout),
			})
		}
		switch {
		case target.Channels < layout.Channels && pol.AllowDownmix:
			faults = append(faults, convFault(loc, "layout", spec.Layout, pol.TargetLayout, "explicit downmix policy"))
			out.Layout = target.Name
		case target.Channels > layout.Channels && pol.AllowUpmix:
			faults = append(faults, convFault(loc, "layout", spec.Layout, pol.TargetLayout, "explicit upmix policy"))
			out.Layout = target.Name
		default:
			faults = append(faults, model.Fault{
				Class: model.FaultAudioLayout, Severity: model.SeverityFatal,
				Location: loc,
				Message:  fmt.Sprintf("layout conflict %s -> %s not permitted by policy", spec.Layout, pol.TargetLayout),
				Expected: pol.TargetLayout, Actual: spec.Layout,
			})
		}
	}

	// Sample-rate normalization.
	if pol.TargetRate != 0 && spec.SampleRate != pol.TargetRate {
		if !validRates[pol.TargetRate] {
			return out, append(faults, model.Fault{
				Class: model.FaultAudioLayout, Severity: model.SeverityFatal,
				Location: loc, Message: fmt.Sprintf("policy targets invalid rate %d", pol.TargetRate),
			})
		}
		if !pol.AllowResample {
			faults = append(faults, model.Fault{
				Class: model.FaultAudioLayout, Severity: model.SeverityFatal,
				Location: loc,
				Message:  fmt.Sprintf("sample-rate conflict %d -> %d not permitted by policy", spec.SampleRate, pol.TargetRate),
				Expected: fmt.Sprintf("%d", pol.TargetRate), Actual: fmt.Sprintf("%d", spec.SampleRate),
			})
		} else {
			faults = append(faults, convFault(loc, "sample_rate",
				fmt.Sprintf("%d", spec.SampleRate), fmt.Sprintf("%d", pol.TargetRate),
				"explicit resample policy"))
			out.SampleRate = pol.TargetRate
		}
	}

	if b := checkBoundary(out, loc); b != nil {
		faults = append(faults, *b)
	}
	return out, faults
}

// checkBoundary verifies that the track duration lands on a whole master-clock
// tick: total samples * MasterRate must be divisible by the sample rate. A
// fractional tick cannot be indexed deterministically.
func checkBoundary(spec Spec, loc model.SourceLocation) *model.Fault {
	if spec.FrameSize == 0 {
		return &model.Fault{
			Class: model.FaultAudioBoundary, Severity: model.SeverityFatal,
			Location: loc, Message: "frame size is zero",
		}
	}
	totalSamples := spec.FrameCount * uint64(spec.FrameSize)
	if totalSamples == 0 {
		return nil
	}
	if (totalSamples*model.MasterRate)%uint64(spec.SampleRate) != 0 {
		return &model.Fault{
			Class: model.FaultAudioBoundary, Severity: model.SeverityFatal,
			Location: loc,
			Message: fmt.Sprintf("duration not divisible: %d samples at %d Hz yields fractional master tick",
				totalSamples, spec.SampleRate),
		}
	}
	return nil
}

func convFault(loc model.SourceLocation, field, from, to, reason string) model.Fault {
	return model.Fault{
		Class: model.FaultAudioLayout, Severity: model.SeverityRecoverable,
		Location: loc, Message: fmt.Sprintf("normalized %s %s -> %s (%s)", field, from, to, reason),
		Expected: to, Actual: from, Inferred: true,
	}
}
