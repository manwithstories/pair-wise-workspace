package audio

import (
	"fmt"
	"strings"

	"avpipe/internal/fault"
)

// Channel identifies a speaker position in a declared layout.
type Channel string

// Canonical channel names used across the pipeline.
const (
	ChFL  Channel = "FL"
	ChFR  Channel = "FR"
	ChFC  Channel = "FC"
	ChLFE Channel = "LFE"
	ChBL  Channel = "BL"
	ChBR  Channel = "BR"
	ChSL  Channel = "SL"
	ChSR  Channel = "SR"
)

// Layout is an ordered channel list. Order is the interleave order on disk.
type Layout struct {
	Name     string    `json:"name"`
	Channels []Channel `json:"channels"`
}

var knownLayouts = []Layout{
	{Name: "mono", Channels: []Channel{ChFC}},
	{Name: "stereo", Channels: []Channel{ChFL, ChFR}},
	{Name: "quad", Channels: []Channel{ChFL, ChFR, ChBL, ChBR}},
	{Name: "5.1", Channels: []Channel{ChFL, ChFR, ChFC, ChLFE, ChBL, ChBR}},
	{Name: "5.1-side", Channels: []Channel{ChFL, ChFR, ChFC, ChLFE, ChSL, ChSR}},
	{Name: "7.1", Channels: []Channel{ChFL, ChFR, ChFC, ChLFE, ChBL, ChBR, ChSL, ChSR}},
}

// ParseLayout resolves a layout name case-insensitively.
func ParseLayout(name string) (Layout, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	for _, l := range knownLayouts {
		if l.Name == key {
			return Layout{Name: l.Name, Channels: append([]Channel(nil), l.Channels...)}, nil
		}
	}
	return Layout{}, fault.New(fault.CodeUnsupportedLayout, "audio.layout",
		"unknown layout %q (known: %s)", name, strings.Join(LayoutNames(), ", "))
}

// LayoutNames lists supported layout names in stable order.
func LayoutNames() []string {
	names := make([]string, len(knownLayouts))
	for i, l := range knownLayouts {
		names[i] = l.Name
	}
	return names
}

// LayoutForChannels returns the canonical layout for a channel count. When
// several layouts share a count the first declared one wins (5.1 over
// 5.1-side), unless a channel mask disambiguates.
func LayoutForChannels(n int) (Layout, error) {
	for _, l := range knownLayouts {
		if len(l.Channels) == n {
			return Layout{Name: l.Name, Channels: append([]Channel(nil), l.Channels...)}, nil
		}
	}
	return Layout{}, fault.New(fault.CodeUnsupportedLayout, "audio.layout",
		"unsupported channel count %d", n)
}

// IndexOf returns the interleave index of ch, or -1.
func (l Layout) IndexOf(ch Channel) int {
	for i, c := range l.Channels {
		if c == ch {
			return i
		}
	}
	return -1
}

// Has reports whether the layout contains ch.
func (l Layout) Has(ch Channel) bool { return l.IndexOf(ch) >= 0 }

// String renders the layout as "name(FL,FR)".
func (l Layout) String() string {
	parts := make([]string, len(l.Channels))
	for i, c := range l.Channels {
		parts[i] = string(c)
	}
	return fmt.Sprintf("%s(%s)", l.Name, strings.Join(parts, ","))
}

// Validate checks a layout is self-consistent.
func (l Layout) Validate() error {
	if len(l.Channels) == 0 {
		return fault.New(fault.CodeUnsupportedLayout, "audio.layout", "layout %q has no channels", l.Name)
	}
	seen := map[Channel]bool{}
	for _, ch := range l.Channels {
		if seen[ch] {
			return fault.New(fault.CodeUnsupportedLayout, "audio.layout",
				"layout %q repeats channel %s", l.Name, ch)
		}
		seen[ch] = true
	}
	return nil
}

// MappingPlan describes how an input layout was mapped to the target.
type MappingPlan struct {
	Input        string    `json:"input"`
	Output       string    `json:"output"`
	Mapped       []Channel `json:"mapped"`
	SilenceFills []Channel `json:"silence_fills"`
}

// MapLayout converts interleaved input samples to the target layout. Target
// channels absent from the input are filled with explicit digital silence;
// channels present in the input but absent from the target are dropped.
// frames samples are expected; len(in) must be frames*len(src.Channels).
func MapLayout(src Layout, in []float64, frames int, target Layout) ([]float64, MappingPlan, error) {
	if err := src.Validate(); err != nil {
		return nil, MappingPlan{}, err
	}
	if err := target.Validate(); err != nil {
		return nil, MappingPlan{}, err
	}
	if frames < 0 || len(in) != frames*len(src.Channels) {
		return nil, MappingPlan{}, fault.New(fault.CodeInvalidInput, "audio.layout",
			"sample buffer has %d values, want %d frames x %d channels", len(in), frames, len(src.Channels))
	}
	out := make([]float64, frames*len(target.Channels))
	plan := MappingPlan{Input: src.Name, Output: target.Name}
	for ti, ch := range target.Channels {
		si := src.IndexOf(ch)
		if si < 0 {
			plan.SilenceFills = append(plan.SilenceFills, ch)
			continue
		}
		plan.Mapped = append(plan.Mapped, ch)
		for f := 0; f < frames; f++ {
			out[f*len(target.Channels)+ti] = in[f*len(src.Channels)+si]
		}
	}
	return out, plan, nil
}

// MaskToLayout maps a WAVE speaker mask to a supported layout.
func MaskToLayout(mask uint32, channels int) (Layout, bool) {
	const fl, fr, fc, lfe, bl, br, sl, sr = 0x1, 0x2, 0x4, 0x8, 0x10, 0x20, 0x200, 0x400
	for _, l := range knownLayouts {
		if len(l.Channels) != channels {
			continue
		}
		var m uint32
		ok := true
		for _, ch := range l.Channels {
			switch ch {
			case ChFL:
				m |= fl
			case ChFR:
				m |= fr
			case ChFC:
				m |= fc
			case ChLFE:
				m |= lfe
			case ChBL:
				m |= bl
			case ChBR:
				m |= br
			case ChSL:
				m |= sl
			case ChSR:
				m |= sr
			default:
				ok = false
			}
		}
		if ok && m == mask {
			return Layout{Name: l.Name, Channels: append([]Channel(nil), l.Channels...)}, true
		}
	}
	return Layout{}, false
}
