// Package normalize reconciles audio parameters (sample rate, channel
// layout) and colour metadata across nodes. Conflicting colour metadata
// quarantines the affected segment instead of silently overwriting.
package normalize

import (
	"fmt"

	"medrelay/internal/timeline"
)

// Target sample rate and layout all audio processing is normalised to.
const (
	TargetRate   = 48000
	TargetLayout = "stereo"
)

// AudioMeta describes one node's audio parameters.
type AudioMeta struct {
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	Layout     string `json:"layout"`
}

// NormalizedAudio is the processing parameter set derived from AudioMeta.
type NormalizedAudio struct {
	ResampleNum int // out_rate/in_rate as a rational
	ResampleDen int
	Channels    int
	Layout      string
}

var layoutChannels = map[string]int{
	"mono":   1,
	"stereo": 2,
	"5.1":    6,
	"7.1":    8,
}

// NormalizeAudio validates the input and computes resample/channel
// parameters toward the pipeline target. Unknown layouts and impossible
// rates are rejected.
func NormalizeAudio(m AudioMeta) (NormalizedAudio, error) {
	ch, ok := layoutChannels[m.Layout]
	if !ok {
		return NormalizedAudio{}, timeline.New(timeline.ErrLimit, "", 0, 0,
			fmt.Sprintf("unknown channel layout %q", m.Layout))
	}
	if m.Channels != ch {
		return NormalizedAudio{}, timeline.New(timeline.ErrLimit, "", 0, 0,
			fmt.Sprintf("layout %q implies %d channels, got %d", m.Layout, ch, m.Channels))
	}
	if m.SampleRate < 8000 || m.SampleRate > 192000 {
		return NormalizedAudio{}, timeline.New(timeline.ErrLimit, "", 0, 0,
			fmt.Sprintf("sample rate %d out of range", m.SampleRate))
	}
	g := gcd(m.SampleRate, TargetRate)
	return NormalizedAudio{
		ResampleNum: TargetRate / g,
		ResampleDen: m.SampleRate / g,
		Channels:    ch,
		Layout:      m.Layout,
	}, nil
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// ColorMeta carries colour information of a segment.
type ColorMeta struct {
	Primaries string `json:"primaries"` // e.g. "bt709", "bt2020"
	Transfer  string `json:"transfer"`  // e.g. "bt709", "pq", "hlg"
	Matrix    string `json:"matrix"`    // e.g. "bt709", "bt2020nc"
	FullRange bool   `json:"full_range"`
}

var (
	validPrimaries = map[string]bool{"bt709": true, "bt2020": true, "smpte432": true}
	validTransfer  = map[string]bool{"bt709": true, "pq": true, "hlg": true}
	validMatrix    = map[string]bool{"bt709": true, "bt2020nc": true}
)

// ValidateColor rejects unknown or empty colour fields.
func ValidateColor(c ColorMeta) error {
	if !validPrimaries[c.Primaries] {
		return timeline.New(timeline.ErrColorConflict, "", 0, 0, "unknown primaries "+c.Primaries)
	}
	if !validTransfer[c.Transfer] {
		return timeline.New(timeline.ErrColorConflict, "", 0, 0, "unknown transfer "+c.Transfer)
	}
	if !validMatrix[c.Matrix] {
		return timeline.New(timeline.ErrColorConflict, "", 0, 0, "unknown matrix "+c.Matrix)
	}
	return nil
}

// ReconcileColor compares the running colour state with the next segment's.
// Equal metadata passes through; a conflict returns quarantine=true with a
// located error so the caller can isolate the segment. The baseline is
// never silently overwritten.
func ReconcileColor(base, next ColorMeta, segment string, startMs, endMs int64) (quarantine bool, err error) {
	if err := ValidateColor(next); err != nil {
		if te, ok := err.(*timeline.Error); ok {
			te.Segment, te.StartMs, te.EndMs = segment, startMs, endMs
		}
		return true, err
	}
	if base == next {
		return false, nil
	}
	return true, timeline.New(timeline.ErrColorConflict, segment, startMs, endMs,
		fmt.Sprintf("colour metadata %+v conflicts with baseline %+v", next, base))
}
