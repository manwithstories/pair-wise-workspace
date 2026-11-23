// Package audio validates channel layouts and sample-rate mapping, records
// unmappable channels, and computes audio timeline offsets.
package audio

import (
	"fmt"

	"mediad/internal/clock"
)

// Track describes one incoming audio track.
type Track struct {
	LayoutName     string
	SampleRate     int
	Channels       []string // per-channel role labels; may contain unknown roles
	PrimingSamples int      // encoder priming delay in samples
}

// Normalized is the validated, unified track description.
type Normalized struct {
	Layout      string
	SampleRate  int
	Channels    []string // mapped roles in canonical order
	Unmapped    []string // roles that could not be mapped
	OffsetTicks int64    // timeline offset in clock.Timescale units
}

// Issue records one non-fatal validation problem.
type Issue struct {
	Kind   string // "layout", "channel", "rate"
	Detail string
}

func (i Issue) String() string { return fmt.Sprintf("%s: %s", i.Kind, i.Detail) }

// layouts maps layout names to canonical channel roles.
var layouts = map[string][]string{
	"mono":   {"FC"},
	"stereo": {"FL", "FR"},
	"5.1":    {"FL", "FR", "FC", "LFE", "SL", "SR"},
	"7.1":    {"FL", "FR", "FC", "LFE", "SL", "SR", "BL", "BR"},
}

// validRates lists accepted sample rates.
var validRates = map[int]bool{44100: true, 48000: true, 96000: true}

// Validate checks the track and returns its normalized form plus issues.
// Unmappable channels are recorded and dropped, never fatal.
func Validate(t Track) (Normalized, []Issue) {
	n := Normalized{Layout: t.LayoutName, SampleRate: t.SampleRate}
	var issues []Issue

	canonical, known := layouts[t.LayoutName]
	if !known {
		issues = append(issues, Issue{"layout", fmt.Sprintf("unknown layout %q; treating channels as discrete", t.LayoutName)})
	}
	if !validRates[t.SampleRate] {
		issues = append(issues, Issue{"rate", fmt.Sprintf("unsupported sample rate %d", t.SampleRate)})
	}
	if known && len(t.Channels) != len(canonical) {
		issues = append(issues, Issue{"layout",
			fmt.Sprintf("layout %q wants %d channels, got %d", t.LayoutName, len(canonical), len(t.Channels))})
	}

	roleOK := map[string]bool{}
	for _, r := range canonical {
		roleOK[r] = true
	}
	for _, ch := range t.Channels {
		if known && !roleOK[ch] {
			n.Unmapped = append(n.Unmapped, ch)
			issues = append(issues, Issue{"channel", fmt.Sprintf("unmappable channel role %q", ch)})
			continue
		}
		n.Channels = append(n.Channels, ch)
	}
	n.OffsetTicks = TimelineOffsetTicks(t.PrimingSamples, t.SampleRate)
	return n, issues
}

// TimelineOffsetTicks converts a priming-sample count to unified-clock ticks.
func TimelineOffsetTicks(primingSamples, sampleRate int) int64 {
	if sampleRate <= 0 || primingSamples <= 0 {
		return 0
	}
	return int64(primingSamples) * clock.Timescale / int64(sampleRate)
}

// MapRate returns the rational resample ratio in/out and whether it is exact.
func MapRate(in, out int) (num, den int, exact bool) {
	if in <= 0 || out <= 0 {
		return 0, 0, false
	}
	g := gcd(in, out)
	return out / g, in / g, in == out
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
