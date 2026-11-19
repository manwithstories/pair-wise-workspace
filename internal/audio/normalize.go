// Package audio normalizes sample rate, channel layout and loudness while
// keeping segment boundaries intact. It reports silence, clipping and empty
// tracks explicitly instead of guessing.
package audio

import (
	"mediamix/internal/core"
	"mediamix/internal/timeline"
)

// Layout is a channel layout in canonical order.
type Layout string

const (
	Mono     Layout = "mono"
	Stereo   Layout = "stereo"
	Layout51 Layout = "5.1"
)

// Channels returns the channel count for a layout.
func (l Layout) Channels() (int, error) {
	switch l {
	case Mono:
		return 1, nil
	case Stereo:
		return 2, nil
	case Layout51:
		return 6, nil
	default:
		return 0, &core.Error{Code: core.CodeBadArgument, Detail: "unsupported channel layout: " + string(l), Fatal: true}
	}
}

// Material is an interleaved PCM segment on the shared timeline.
type Material struct {
	Track  string        `json:"track"`
	Rate   int           `json:"rate"`
	Layout Layout        `json:"layout"`
	Span   timeline.Span `json:"span"`
	// Samples is interleaved, len == frames*channels.
	Samples []float64 `json:"-"`
}

// Frames returns the frame count.
func (m *Material) Frames() int {
	ch, err := m.Layout.Channels()
	if err != nil || ch == 0 {
		return 0
	}
	return len(m.Samples) / ch
}

// Validate rejects material whose time base cannot be determined.
func (m *Material) Validate() (*core.Error, error) {
	if m == nil {
		return nil, &core.Error{Code: core.CodeBadArgument, Detail: "nil audio material", Fatal: true}
	}
	if m.Track == "" {
		return nil, &core.Error{Code: core.CodeBadArgument, Detail: "audio track id is empty", Fatal: true}
	}
	if m.Rate <= 0 {
		return nil, &core.Error{Code: core.CodeBadTimestamp, Track: m.Track, Detail: "sample rate is not positive", Fatal: true}
	}
	if _, err := m.Layout.Channels(); err != nil {
		return nil, core.StampTrack(err, m.Track)
	}
	if !m.Span.Valid() {
		return nil, &core.Error{Code: core.CodeBadTimestamp, Track: m.Track, Detail: "invalid audio span", Fatal: true}
	}
	if len(m.Samples) == 0 {
		return &core.Error{Code: core.CodeEmptyTrack, Track: m.Track, Detail: "audio material has no samples", Fatal: false}, nil
	}
	return nil, nil
}

// Target describes the unified output audio specification.
type Target struct {
	Rate         int     `json:"rate"`
	Layout       Layout  `json:"layout"`
	LoudnessLUFS float64 `json:"loudness_lufs"` // e.g. -23 for EBU R128
}

// DefaultTarget is the proxy deliverable: 48 kHz stereo at -23 LUFS.
func DefaultTarget() Target { return Target{Rate: 48000, Layout: Stereo, LoudnessLUFS: -23} }

// Result is the normalized material plus explicit status flags.
type Result struct {
	Track        string        `json:"track"`
	Span         timeline.Span `json:"span"`
	Rate         int           `json:"rate"`
	Layout       Layout        `json:"layout"`
	Frames       int           `json:"frames"`
	InputPeakDB  float64       `json:"input_peak_db"`
	OutputPeakDB float64       `json:"output_peak_db"`
	GainDB       float64       `json:"gain_db"`
	Silent       bool          `json:"silent"`
	Clipped      bool          `json:"clipped"`
	Resampled    bool          `json:"resampled"`
	Upmixed      bool          `json:"upmixed"`
	// Status carries non-fatal notices such as silence or clipping.
	Status   []*core.Error `json:"status,omitempty"`
	Material *Material     `json:"-"`
}

// Normalize converts material to the target spec without crossing its span
// boundaries: output durations match input durations to within one sample.
func Normalize(m *Material, target Target) (*Result, error) {
	if m == nil {
		return nil, &core.Error{Code: core.CodeBadArgument, Detail: "nil audio material", Fatal: true}
	}
	if warn, err := m.Validate(); err != nil {
		return nil, err
	} else if warn != nil {
		// Empty track: return a result that clearly says so, not an error.
		return &Result{Track: m.Track, Span: m.Span, Rate: target.Rate, Layout: target.Layout,
			Silent: true, Status: []*core.Error{warn}, Material: &Material{
				Track: m.Track, Rate: target.Rate, Layout: target.Layout, Span: m.Span,
			}}, nil
	}
	if target.Rate <= 0 {
		return nil, &core.Error{Code: core.CodeBadArgument, Track: m.Track, Detail: "target sample rate is not positive", Fatal: true}
	}
	if _, err := target.Layout.Channels(); err != nil {
		return nil, core.StampTrack(err, m.Track)
	}

	res := &Result{Track: m.Track, Span: m.Span, Rate: target.Rate, Layout: target.Layout}
	peak := peakAbs(m.Samples)
	res.InputPeakDB = toDB(peak)
	if peak == 0 {
		res.Silent = true
		res.Status = append(res.Status, &core.Error{Code: core.CodeSilentAudio, Track: m.Track,
			Detail: "input is digital silence", Fatal: false})
	}
	// Clipping is detected before gain: a sample at or above full scale in the
	// input is reported even if loudness normalization would later reduce it.
	if peak >= 0.999 {
		res.Clipped = true
		res.Status = append(res.Status, &core.Error{Code: core.CodeClippedAudio, Track: m.Track,
			Detail: "input reaches full scale", Fatal: false})
	}

	layout := m
	if m.Layout != target.Layout {
		changed, err := remapLayout(m, target.Layout)
		if err != nil {
			return nil, err
		}
		layout = changed
		res.Upmixed = true
	}
	resampled := layout
	if layout.Rate != target.Rate {
		changed, err := resampleLinear(layout, target.Rate)
		if err != nil {
			return nil, err
		}
		resampled = changed
		res.Resampled = true
	}

	loud := loudness(resampled.Samples, resampled.Layout)
	res.GainDB = gainFor(loud, target.LoudnessLUFS, res.Silent)
	if !res.Silent {
		applyGain(resampled.Samples, res.GainDB)
	}
	// Prevent the gain from pushing the deliverable past full scale.
	if p := peakAbs(resampled.Samples); p > 1 {
		limit := toDB(1.0 / p)
		applyGain(resampled.Samples, limit)
		res.GainDB += limit
	}
	res.OutputPeakDB = toDB(peakAbs(resampled.Samples))
	res.Frames = resampled.Frames()
	res.Material = resampled
	return res, nil
}

// gainFor maps measured loudness onto the target with a bounded correction.
// Silence is returned unchanged: boosting digital silence is meaningless and
// the caller already flagged it.
func gainFor(loudness, target float64, silent bool) float64 {
	if silent {
		return 0
	}
	g := target - loudness
	if g > 24 {
		g = 24
	}
	if g < -24 {
		g = -24
	}
	return g
}
