// Package audio validates channel layouts, sample rates and resampling
// boundaries before a segment plan is allowed to commit.
package audio

import (
	"fmt"

	"github.com/example/mediad/internal/ingest"
	"github.com/example/mediad/internal/model"
)

// supportedLayouts is the allow-list of channel label orders the
// pipeline can map without an external downmix/upmix engine.
var supportedLayouts = map[string]bool{
	"M":         true, // mono
	"L|R":       true, // stereo
	"L|R|C":     true, // 3.0
	"L|R|LFE":   true, // 2.1
	"L|R|C|LFE": true, // 3.1
}

// standardRates are the rates the delivery targets accept directly.
var standardRates = map[uint32]bool{
	8000: true, 16000: true, 32000: true, 44100: true, 48000: true,
	96000: true,
}

// Mapping is the validated plan for one audio track.
type Mapping struct {
	TrackID       uint32
	InputRate     uint32
	OutputRate    uint32
	Channels      uint16
	Layout        []string
	LayoutKey     string
	NeedsResample bool
	// RejectedRanges lists sample intervals that cannot be mapped
	// safely; empty means the whole track is acceptable.
	RejectedRanges [][2]uint64
}

// ValidateLayout checks a track's channel count against its declared
// labels and the allow-list.
func ValidateLayout(a *ingest.AudioTrackLayout) error {
	if a == nil {
		return model.New(model.KindArgument, "audio", "layout",
			"nil audio track")
	}
	if a.Channels == 0 {
		return model.New(model.KindUnsupported, "audio", "layout",
			"zero-channel audio track")
	}
	if len(a.ChannelLayout) != int(a.Channels) {
		return model.New(model.KindUnsupported, "audio", "layout",
			fmt.Sprintf("channel count %d does not match labels %v",
				a.Channels, a.ChannelLayout))
	}
	key := layoutKey(a.ChannelLayout)
	if !supportedLayouts[key] {
		return model.New(model.KindUnsupported, "audio", "layout",
			"unsupported channel layout: "+key)
	}
	return nil
}

// PlanMapping validates a track and computes its output mapping. The
// output rate defaults to the input rate when it is a standard rate;
// otherwise 48 kHz is selected and resampling is required.
func PlanMapping(a *ingest.AudioTrackLayout) (*Mapping, error) {
	if err := ValidateLayout(a); err != nil {
		return nil, err
	}
	if a.SampleRate == 0 {
		return nil, model.New(model.KindUnsupported, "audio", "layout",
			"zero sample rate")
	}
	m := &Mapping{
		TrackID:    a.TrackID,
		InputRate:  a.SampleRate,
		OutputRate: a.SampleRate,
		Channels:   a.Channels,
		Layout:     append([]string(nil), a.ChannelLayout...),
		LayoutKey:  layoutKey(a.ChannelLayout),
	}
	if !standardRates[a.SampleRate] {
		m.OutputRate = 48000
		m.NeedsResample = true
	}
	return m, nil
}

// CheckResampleBoundary verifies that every planned sample interval
// converts to a whole number of output samples. Intervals that do not
// divide evenly are recorded as rejected and the function returns a
// classified error naming the offending sample range.
func (m *Mapping) CheckResampleBoundary(ranges [][2]uint64) error {
	if !m.NeedsResample {
		// Passthrough: every boundary is exact.
		return nil
	}
	var bad [][2]uint64
	for _, r := range ranges {
		from, to := r[0], r[1]
		if to < from {
			return model.New(model.KindArgument, "audio", "resample",
				fmt.Sprintf("inverted sample range [%d,%d]", from, to))
		}
		span := to - from + 1
		out, rem := convertCount(span, m.InputRate, m.OutputRate)
		_ = out
		if rem != 0 {
			bad = append(bad, r)
		}
	}
	m.RejectedRanges = bad
	if len(bad) > 0 {
		return model.New(model.KindUnsupported, "audio", "resample",
			fmt.Sprintf("indivisible resample boundary in ranges %v", bad))
	}
	return nil
}

// convertCount maps an input sample count to the output rate, returning
// the whole output count and a nonzero remainder when the boundary does
// not land on an exact output sample.
func convertCount(inputCount uint64, inRate, outRate uint32) (uint64, uint64) {
	num := inputCount * uint64(outRate)
	den := uint64(inRate)
	return num / den, num % den
}

func layoutKey(labels []string) string {
	key := ""
	for i, l := range labels {
		if i > 0 {
			key += "|"
		}
		key += l
	}
	return key
}
