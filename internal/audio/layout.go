// Package audio resolves channel layouts and sample rates across the tracks
// that make up one program.  When two descriptions disagree, mediapack never
// guesses: the operator either supplies an explicit mapping policy or the
// stage blocks and names the first conflicting dependency.
package audio

import (
	"fmt"
	"sort"
	"strings"

	"mediapack/internal/fault"
)

// Layout is a speaker layout understood by the tool.
type Layout string

const (
	LayoutMono    Layout = "mono"
	LayoutStereo  Layout = "stereo"
	Layout5_1     Layout = "5.1"
	Layout5_1_2   Layout = "5.1.2"
	LayoutAtmos   Layout = "atmos"
	LayoutUnknown Layout = "unknown"
)

// channelMap maps canonical channel names to their 0-based index in a layout.
// Names follow the common FFmpeg/Vorbis ordering so operators can compare
// with ffprobe output directly.
var channelMap = map[Layout][]string{
	LayoutMono:   {"FC"},
	LayoutStereo: {"FL", "FR"},
	Layout5_1:    {"FL", "FR", "FC", "LFE", "BL", "BR"},
	Layout5_1_2:  {"FL", "FR", "FC", "LFE", "BL", "BR", "TFL", "TFR"},
	LayoutAtmos:  {"FL", "FR", "FC", "LFE", "BL", "BR", "TFL", "TFR", "TBL", "TBR"},
}

// Channels returns the ordered channel identifiers, or nil when unknown.
func Channels(l Layout) []string {
	ch, ok := channelMap[l]
	if !ok {
		return nil
	}
	out := make([]string, len(ch))
	copy(out, ch)
	return out
}

// Count returns the channel count, or 0 when the layout is unknown.
func Count(l Layout) int { return len(channelMap[l]) }

// ParseLayout normalizes an operator-supplied layout name.
func ParseLayout(s string) Layout {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "mono", "1.0", "m":
		return LayoutMono
	case "stereo", "2.0", "l/r":
		return LayoutStereo
	case "5.1", "51":
		return Layout5_1
	case "5.1.2", "512":
		return Layout5_1_2
	case "atmos", "7.1.2", "7.1.4":
		return LayoutAtmos
	default:
		return LayoutUnknown
	}
}

// Stream is one audio stream's description as reported by its container.
type Stream struct {
	Track      string
	Layout     Layout
	SampleRate uint32
	// Source identifies the upstream file or station, preserved verbatim in
	// conflict reports so a disagreement can be traced to its producer.
	Source string
}

// Policy selects how a disagreement is resolved.  PolicyBlock is the zero
// value and therefore the safe default.
type Policy string

const (
	// PolicyBlock refuses to proceed and reports the first conflict.
	PolicyBlock Policy = "block"
	// PolicyPrefer keeps the channel layout of the named source.
	PolicyPrefer Policy = "prefer"
	// PolicyMapDown converts to the target layout using a fixed channel map.
	PolicyMapDown Policy = "downmix"
)

// Config is the explicit operator decision for one program.
type Config struct {
	Policy       Policy
	PreferSource string
	Target       Layout
}

// Resolution is the reconciled layout for the program.
type Resolution struct {
	Layout          Layout
	SampleRate      uint32
	Source          string
	Mappings        map[string][]ChannelMap
	Conflicts       []Conflict
	RateConflicts   []Conflict
	AppliedPolicy   Policy
	MappingRequired bool
}

// ChannelMap says where one program channel comes from.
type ChannelMap struct {
	TargetChannel string
	SourceTrack   string
	SourceChannel string
	// Mixed is set when several source channels feed one target channel.
	Mixed bool
}

// Conflict records one disagreement without discarding its source.
type Conflict struct {
	Kind   string // "layout" or "sample-rate"
	Field  string // layout / sample-rate value
	Source string
	Detail string
}

// Limits bounds the number of audio tracks accepted.
type Limits struct{ MaxTracks int }

// DefaultLimits mirrors the frame limits philosophy: small and explicit.
func DefaultLimits() Limits { return Limits{MaxTracks: 32} }

// Resolve reconciles streams.  It returns a resolution when the inputs are
// consistent or the policy resolves them, and a dependency fault naming the
// first conflicting track otherwise.
func Resolve(streams []Stream, cfg Config, limits Limits) (*Resolution, error) {
	if len(streams) == 0 {
		return nil, fault.New(fault.ClassInput, "audio", "*", "no-audio", "no audio streams supplied")
	}
	if len(streams) > limits.MaxTracks {
		return nil, fault.New(fault.ClassLimit, "audio", "*", "too-many-tracks",
			fmt.Sprintf("%d audio tracks exceeds limit %d", len(streams), limits.MaxTracks))
	}
	for _, s := range streams {
		if s.Track == "" {
			return nil, fault.New(fault.ClassInput, "audio", "*", "invalid-stream", "audio stream has empty track name")
		}
		if s.Layout == LayoutUnknown {
			return nil, fault.New(fault.ClassInput, "audio", s.Track, "unknown-layout",
				"audio stream declares an unsupported channel layout")
		}
		if s.SampleRate == 0 {
			return nil, fault.New(fault.ClassInput, "audio", s.Track, "invalid-sample-rate",
				"audio stream declares a zero sample rate")
		}
	}

	ordered := append([]Stream(nil), streams...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Track < ordered[j].Track })

	program := ordered[0]
	baseline := program
	res := &Resolution{Layout: program.Layout, SampleRate: program.SampleRate, Source: program.Source,
		Mappings: make(map[string][]ChannelMap)}

	for _, s := range ordered[1:] {
		if s.Layout != baseline.Layout {
			res.Conflicts = append(res.Conflicts, Conflict{
				Kind: "layout", Field: string(s.Layout), Source: s.Source,
				Detail: fmt.Sprintf("%s declares %s, program has %s", s.Track, s.Layout, baseline.Layout),
			})
		}
		if s.SampleRate != baseline.SampleRate {
			res.RateConflicts = append(res.RateConflicts, Conflict{
				Kind: "sample-rate", Field: fmt.Sprintf("%d", s.SampleRate), Source: s.Source,
				Detail: fmt.Sprintf("%s declares %d Hz, program has %d Hz", s.Track, s.SampleRate, baseline.SampleRate),
			})
		}
	}

	switch cfg.Policy {
	case PolicyPrefer:
		chosen, ok := findSource(ordered, cfg.PreferSource)
		if !ok {
			return nil, fault.New(fault.ClassUsage, "audio", cfg.PreferSource, "prefer-source-missing",
				"preferred source does not name a supplied audio track")
		}
		program = chosen
		res.Layout, res.SampleRate, res.Source = chosen.Layout, chosen.SampleRate, chosen.Source
		res.AppliedPolicy = PolicyPrefer
	case PolicyMapDown:
		target := cfg.Target
		if target == LayoutUnknown {
			target = LayoutStereo
		}
		if Count(target) == 0 {
			return nil, fault.New(fault.ClassUsage, "audio", "*", "unsupported-target",
				"downmix target layout is not supported")
		}
		program = Stream{Track: program.Track, Layout: target, SampleRate: program.SampleRate, Source: program.Source}
		res.Layout, res.Source = target, program.Source
		res.AppliedPolicy = PolicyMapDown
	case PolicyBlock, "":
		res.AppliedPolicy = PolicyBlock
		if len(res.Conflicts) > 0 {
			c := res.Conflicts[0]
			return nil, fault.New(fault.ClassDependency, "audio", c.Source, "layout-conflict", c.Detail).
				WithBlockedBy("audio:" + c.Source)
		}
		if len(res.RateConflicts) > 0 {
			c := res.RateConflicts[0]
			return nil, fault.New(fault.ClassDependency, "audio", c.Source, "sample-rate-conflict", c.Detail).
				WithBlockedBy("audio:" + c.Source)
		}
		return res, nil
	default:
		return nil, fault.New(fault.ClassUsage, "audio", "*", "unknown-policy",
			fmt.Sprintf("unknown audio policy %q", cfg.Policy))
	}

	// An explicit policy was applied: every stream must map into the program.
	target := Stream{Track: program.Track, Layout: program.Layout, Source: program.Source}
	for _, s := range ordered {
		if s.Layout == program.Layout {
			continue
		}
		mapping := MapChannels(s, target)
		if len(mapping) == 0 {
			return nil, fault.New(fault.ClassDependency, "audio", s.Track, "missing-mapping",
				fmt.Sprintf("%s (%s) cannot be mapped to %s", s.Track, s.Layout, program.Layout)).
				WithBlockedBy("audio:" + s.Track)
		}
		res.Mappings[s.Track] = mapping
		res.MappingRequired = true
	}
	return res, nil
}

func findSource(streams []Stream, source string) (Stream, bool) {
	for _, s := range streams {
		if s.Source == source || s.Track == source {
			return s, true
		}
	}
	return Stream{}, false
}

// MapChannels builds an explicit mapping from src's layout to dst's layout.
// A source channel that has no counterpart is reported by an empty mapping:
// callers must treat len(result)==0 as "no safe mapping" and block.
func MapChannels(src, dst Stream) []ChannelMap {
	srcCh := Channels(src.Layout)
	dstCh := Channels(dst.Layout)
	if len(srcCh) == 0 || len(dstCh) == 0 {
		return nil
	}
	index := make(map[string]int, len(srcCh))
	for i, name := range srcCh {
		index[name] = i
	}
	out := make([]ChannelMap, 0, len(dstCh))
	for _, target := range dstCh {
		if i, ok := index[target]; ok {
			out = append(out, ChannelMap{TargetChannel: target, SourceTrack: src.Track, SourceChannel: srcCh[i]})
			continue
		}
		// Stereo targets fold the center into both sides; this mirrors the
		// standard downmix matrix and keeps the mapping explicit.  Any other
		// missing channel is not invented.
		switch target {
		case "FL", "FR":
			if i, ok := index["FC"]; ok {
				out = append(out, ChannelMap{TargetChannel: target, SourceTrack: src.Track, SourceChannel: srcCh[i], Mixed: true})
				continue
			}
		}
		return nil
	}
	return out
}

// Summary renders a stable one-line description used in manifests.
func (r *Resolution) Summary() string {
	parts := []string{string(r.Layout), fmt.Sprintf("%dHz", r.SampleRate)}
	if r.MappingRequired {
		parts = append(parts, "mapped")
	}
	if n := len(r.Conflicts) + len(r.RateConflicts); n > 0 {
		parts = append(parts, fmt.Sprintf("conflicts=%d", n))
	}
	return strings.Join(parts, ",")
}
