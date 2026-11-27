// Package metadata adjudicates display metadata: rotation, crop and color.
//
// The same information can be declared by the container, by the track
// (stream-level metadata or side data) and by a codec bitstream (e.g. an H.264
// VUI). When those disagree a receiver cannot know which to trust. This package
// applies one fixed precedence — explicit configuration, then track, then
// container, then bitstream — and records every conflict it saw so the
// decision is auditable instead of silently inconsistent.
package metadata

import (
	"fmt"
	"sort"
	"strings"

	"avpipe/internal/media"
)

// Source identifies where a piece of display metadata came from.
type Source string

const (
	SourceExplicit  Source = "explicit"
	SourceTrack     Source = "track"
	SourceContainer Source = "container"
	SourceBitstream Source = "bitstream"
)

// precedence returns a lower number for higher priority.
func precedence(s Source) int {
	switch s {
	case SourceExplicit:
		return 0
	case SourceTrack:
		return 1
	case SourceContainer:
		return 2
	case SourceBitstream:
		return 3
	default:
		return 9
	}
}

// ColorSource is one color declaration from one source. Empty fields mean the
// source did not specify that field.
type ColorSource struct {
	Source Source
	Color  media.ColorInfo
}

// Explicit is operator-supplied configuration; nil fields mean "not set".
type Explicit struct {
	Rotation *int
	Crop     *media.CropRect
	Color    *media.ColorInfo
}

// VideoFacts collects everything the demuxer learned about one video track
// before adjudication.
type VideoFacts struct {
	TrackID string
	Width   int // coded width
	Height  int // coded height
	// Rotation from the stream side data (display matrix), degrees.
	TrackRotation int
	HasTrackRot   bool
	// Crop from stream-level metadata, if declared.
	TrackCrop *media.CropRect
	// Rotation and crop declared by the container wrapper.
	ContainerRotation int
	HasContainerRot   bool
	ContainerCrop     *media.CropRect
	// Color declarations, any order; precedence is applied here.
	Color []ColorSource
	Origin media.Origin
}

// Resolve applies the precedence rules and returns a fully decided DisplayInfo.
// An illegal rotation or an out-of-bounds crop marks the track invalid and
// returns a policy error so the caller can disable the track explicitly.
func Resolve(f VideoFacts, ex Explicit) (media.DisplayInfo, error) {
	di := media.DisplayInfo{Provenance: map[string]string{}}
	var problems []string

	rot, rotSrc, rotOK := pickRotation(f, ex)
	if rotOK {
		di.Rotation = rot
		di.Provenance["rotation"] = string(rotSrc)
	} else if rotSrc != "" {
		problems = append(problems, fmt.Sprintf("rotation %d from %s is not a multiple of 90", rot, rotSrc))
	}

	crop, cropSrc, cropErr := pickCrop(f, ex)
	if cropErr == nil && crop != nil {
		if err := validateCrop(*crop, f.Width, f.Height); err != nil {
			problems = append(problems, err.Error())
		} else {
			di.Crop = crop
			di.Provenance["crop"] = string(cropSrc)
		}
	} else if cropErr != nil {
		problems = append(problems, cropErr.Error())
	}

	// Explicit color configuration is the highest-priority source and is
	// folded into the candidate list so conflicts against lower sources are
	// still reported.
	sources := make([]ColorSource, 0, len(f.Color)+1)
	if ex.Color != nil {
		sources = append(sources, ColorSource{Source: SourceExplicit, Color: *ex.Color})
	}
	sources = append(sources, f.Color...)
	di.Color, di.Conflicts = resolveColor(sources)
	for field, src := range colorProvenance(sources) {
		di.Provenance[field] = string(src)
	}

	if len(problems) > 0 {
		di.Invalid = true
		di.InvalidReason = strings.Join(problems, "; ")
		kind := media.KindPolicy
		if strings.Contains(di.InvalidReason, "crop") || strings.Contains(di.InvalidReason, "rotation") {
			kind = media.KindCorrupt
		}
		return di, media.Errorf(kind, f.Origin, "metadata.Resolve",
			"track %s display metadata invalid: %s", f.TrackID, di.InvalidReason)
	}
	return di, nil
}

// pickRotation applies precedence to rotation candidates and normalises the
// winner into [0,360). The bool is false when a candidate existed but was
// illegal; an empty source means no candidate was present at all.
func pickRotation(f VideoFacts, ex Explicit) (int, Source, bool) {
	type cand struct {
		v   int
		src Source
	}
	var cands []cand
	if ex.Rotation != nil {
		cands = append(cands, cand{*ex.Rotation, SourceExplicit})
	}
	if f.HasTrackRot {
		cands = append(cands, cand{f.TrackRotation, SourceTrack})
	}
	if f.HasContainerRot {
		cands = append(cands, cand{f.ContainerRotation, SourceContainer})
	}
	if len(cands) == 0 {
		return 0, "", true
	}
	sort.SliceStable(cands, func(i, j int) bool { return precedence(cands[i].src) < precedence(cands[j].src) })
	c := cands[0]
	n := ((c.v % 360) + 360) % 360
	if n%90 != 0 {
		return n, c.src, false
	}
	return n, c.src, true
}

func pickCrop(f VideoFacts, ex Explicit) (*media.CropRect, Source, error) {
	var c *media.CropRect
	var src Source
	if ex.Crop != nil {
		c, src = ex.Crop, SourceExplicit
	} else if f.TrackCrop != nil {
		c, src = f.TrackCrop, SourceTrack
	} else if f.ContainerCrop != nil {
		c, src = f.ContainerCrop, SourceContainer
	}
	if c != nil {
		cv := *c // copy so callers cannot mutate the decision
		return &cv, src, nil
	}
	return nil, "", nil
}

func validateCrop(c media.CropRect, w, h int) error {
	if c.W <= 0 || c.H <= 0 {
		return fmt.Errorf("crop %dx%d has non-positive size", c.W, c.H)
	}
	if c.X < 0 || c.Y < 0 {
		return fmt.Errorf("crop origin %d,%d is negative", c.X, c.Y)
	}
	if c.X+c.W > w || c.Y+c.H > h {
		return fmt.Errorf("crop %dx%d+%d+%d exceeds coded frame %dx%d", c.W, c.H, c.X, c.Y, w, h)
	}
	return nil
}

// colorFields maps canonical field names to accessors so precedence can be
// applied uniformly.
var colorFields = []struct {
	name string
	get  func(media.ColorInfo) string
	set  func(*media.ColorInfo, string)
}{
	{"color.primaries", func(c media.ColorInfo) string { return c.Primaries }, func(c *media.ColorInfo, v string) { c.Primaries = v }},
	{"color.transfer", func(c media.ColorInfo) string { return c.Transfer }, func(c *media.ColorInfo, v string) { c.Transfer = v }},
	{"color.space", func(c media.ColorInfo) string { return c.Space }, func(c *media.ColorInfo, v string) { c.Space = v }},
	{"color.range", func(c media.ColorInfo) string { return c.Range }, func(c *media.ColorInfo, v string) { c.Range = v }},
}

// resolveColor picks each color field independently and reports conflicts for
// every lower-priority source that declared a different value.
func resolveColor(sources []ColorSource) (media.ColorInfo, []string) {
	var out media.ColorInfo
	var conflicts []string
	sorted := make([]ColorSource, len(sources))
	copy(sorted, sources)
	sort.SliceStable(sorted, func(i, j int) bool { return precedence(sorted[i].Source) < precedence(sorted[j].Source) })
	for _, f := range colorFields {
		var winner string
		var winnerSrc Source
		for _, s := range sorted {
			if v := f.get(s.Color); v != "" {
				winner, winnerSrc = v, s.Source
				break
			}
		}
		if winner == "" {
			continue
		}
		f.set(&out, winner)
		for _, s := range sorted {
			v := f.get(s.Color)
			if v == "" || s.Source == winnerSrc {
				continue
			}
			if v != winner {
				conflicts = append(conflicts, fmt.Sprintf("%s: %s=%s (ignored, %s=%s won)",
					f.name, s.Source, v, winnerSrc, winner))
			}
		}
	}
	return out, conflicts
}

// colorProvenance records which source supplied each resolved field.
func colorProvenance(sources []ColorSource) map[string]Source {
	out := map[string]Source{}
	sorted := make([]ColorSource, len(sources))
	copy(sorted, sources)
	sort.SliceStable(sorted, func(i, j int) bool { return precedence(sorted[i].Source) < precedence(sorted[j].Source) })
	for _, f := range colorFields {
		for _, s := range sorted {
			if v := f.get(s.Color); v != "" {
				out[f.name] = s.Source
				break
			}
		}
	}
	return out
}
