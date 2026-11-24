// Package metadata preserves raw audio channel/sample-rate and video color
// values and decides track compatibility. Conflicts split audio groups or
// mark video representations incompatible; nothing is resampled or rewritten.
package metadata

import (
	"fmt"
	"sort"
	"strings"
)

// Audio carries the raw audio track description. Values are preserved exactly
// as signalled; no resampling or downmix is ever performed.
type Audio struct {
	Channels        uint16 // raw channelcount
	SampleRateRaw   uint32 // raw 16.16 fixed-point samplerate field
	SampleRateHz    uint32 // integer Hz decoded from SampleRateRaw
	LayoutCode      uint32 // raw speaker-layout code (spkl box)
	Layout          string // speaker label list, e.g. "L R C LFE Ls Rs"
}

// Color carries raw ISO nclx color metadata.
type Color struct {
	ColorType string // e.g. "nclx"
	Primaries uint16 // colour_primaries raw
	Transfer  uint16 // transfer_characteristics raw
	Matrix    uint16 // matrix_coefficients raw
	FullRange bool   // full_range_flag raw
}

// Descriptor is the per-representation metadata input to policy.
type Descriptor struct {
	Representation string
	Audio          *Audio
	Color          *Color
}

// AudioGroup is a set of representations sharing an identical raw audio
// layout and rate. Different raw values produce separate groups.
type AudioGroup struct {
	Key            string
	Channels       uint16
	SampleRateRaw  uint32
	SampleRateHz   uint32
	Layout         string
	Representations []string
}

// GroupAudio splits representations into compatibility groups by the exact
// raw channel layout and sample rate. Groups and members are sorted.
func GroupAudio(descs []Descriptor) []AudioGroup {
	type agg struct {
		g    AudioGroup
		reps []string
	}
	m := map[string]*agg{}
	var keys []string
	for _, d := range descs {
		if d.Audio == nil {
			continue
		}
		a := d.Audio
		key := fmt.Sprintf("ch=%d/srraw=%d/layout=%s", a.Channels, a.SampleRateRaw, a.Layout)
		cur, ok := m[key]
		if !ok {
			keys = append(keys, key)
			m[key] = &agg{g: AudioGroup{
				Key:           key,
				Channels:      a.Channels,
				SampleRateRaw: a.SampleRateRaw,
				SampleRateHz:  a.SampleRateHz,
				Layout:        a.Layout,
			}}
			cur = m[key]
		}
		cur.reps = append(cur.reps, d.Representation)
	}
	sort.Strings(keys)
	groups := make([]AudioGroup, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		sort.Strings(a.reps)
		a.g.Representations = a.reps
		groups = append(groups, a.g)
	}
	return groups
}

// ColorGroup bundles one raw color tuple with the representations using it.
type ColorGroup struct {
	Color           Color
	Representations []string
}

// VideoReport separates the dominant exactly-matching color set from
// representations whose raw color differs and must not be switched into.
type VideoReport struct {
	Compatible   []string
	Incompatible []string
	Groups       []ColorGroup
	Reference    *Color
}

// colorKey renders a color tuple deterministically.
func colorKey(c *Color) string {
	return fmt.Sprintf("%s/%d/%d/%d/%t", c.ColorType, c.Primaries, c.Transfer, c.Matrix, c.FullRange)
}

// CheckVideo groups representations by exact raw color values and elects a
// deterministic reference group: largest group wins, ties broken by the
// smallest color key. Everyone outside it is marked incompatible.
func CheckVideo(descs []Descriptor) VideoReport {
	m := map[string]*ColorGroup{}
	var keys []string
	for _, d := range descs {
		if d.Color == nil {
			continue
		}
		k := colorKey(d.Color)
		g, ok := m[k]
		if !ok {
			keys = append(keys, k)
			cp := *d.Color
			m[k] = &ColorGroup{Color: cp}
			g = m[k]
		}
		g.Representations = append(g.Representations, d.Representation)
	}
	sort.Strings(keys)

	groups := make([]ColorGroup, 0, len(keys))
	var refKey string
	bestN := -1
	for _, k := range keys {
		g := m[k]
		sort.Strings(g.Representations)
		groups = append(groups, *g)
		if len(g.Representations) > bestN || (len(g.Representations) == bestN && k < refKey) {
			bestN = len(g.Representations)
			refKey = k
		}
	}
	rep := VideoReport{Groups: groups}
	if refKey == "" {
		return rep
	}
	ref := m[refKey].Color
	rep.Reference = &ref
	for _, g := range groups {
		if colorKey(&g.Color) == refKey {
			rep.Compatible = append(rep.Compatible, g.Representations...)
		} else {
			rep.Incompatible = append(rep.Incompatible, g.Representations...)
		}
	}
	sort.Strings(rep.Compatible)
	sort.Strings(rep.Incompatible)
	return rep
}

// Summary renders stable key=value tokens for logs/manifests.
func (r VideoReport) String() string {
	return fmt.Sprintf("video compatible=[%s] incompatible=[%s]",
		strings.Join(r.Compatible, ","), strings.Join(r.Incompatible, ","))
}
