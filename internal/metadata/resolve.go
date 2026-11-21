// Package metadata resolves geometry and colour descriptions whose sources
// can disagree: the container, the track and an explicit operator override.
package metadata

import (
	"fmt"
	"sort"

	"mediacore/internal/media"
)

// Override is the explicit, operator-supplied description. It always wins,
// and every field it supplies is recorded as the conflict source so the
// manifest can show why a value changed.
type Override struct {
	Rotation *int
	Crop     *media.Crop
	Color    *media.Color
	// RejectRotation, when true, refuses a non-integral rotation instead of
	// normalizing it.
	StrictRotation bool
}

// Resolved is the final description of one track.
type Resolved struct {
	TrackKey     string
	Rotation     int
	RotationFrom string
	// RotationConflicts lists the other sources that disagreed, in
	// container, track, override order.
	RotationConflicts []string
	Crop              *media.Crop
	CropFrom          string
	Color             media.Color
	ColorFrom         string
	ColorConflicts    []string
	// Warnings are non-fatal notes, such as a colour description that was
	// unknown and replaced by the documented default.
	Warnings []string
}

// Result is the resolution for a whole container.
type Result struct {
	Entries []Resolved
}

// RotationFromMatrixValue normalizes any integer rotation to 0..3 quarter
// turns. Values outside 0..3 are reduced, which is what a display matrix with
// 450 degrees means in practice.
func normalizeRotation(n int) int {
	m := n % 4
	if m < 0 {
		m += 4
	}
	return m
}

// Resolve applies the documented priority: override, then track, then
// container, then the documented defaults. It never silently drops a
// disagreeing source; every conflict is recorded with its origin.
func Resolve(c *media.Container, ov Override) (*Result, error) {
	res := &Result{}
	for _, t := range c.Tracks {
		r, err := resolveTrack(c, t, ov)
		if err != nil {
			return nil, err
		}
		res.Entries = append(res.Entries, r)
	}
	sort.SliceStable(res.Entries, func(i, j int) bool {
		return res.Entries[i].TrackKey < res.Entries[j].TrackKey
	})
	return res, nil
}

func resolveTrack(c *media.Container, t *media.Track, ov Override) (Resolved, error) {
	out := Resolved{TrackKey: t.ID.String()}

	// --- rotation ---
	rotationSources := []struct {
		value   int
		label   string
		present bool
	}{
		{value: c.Rotation, label: "container", present: c.HasRotate},
		{value: t.Rotation, label: "track", present: t.HasRotate},
	}
	final, finalLabel := 0, "default"
	if ov.Rotation != nil {
		final, finalLabel = normalizeRotation(*ov.Rotation), "override"
	} else {
		for _, s := range rotationSources {
			if s.present {
				if ov.StrictRotation && s.value != normalizeRotation(s.value) {
					return out, media.NewError(media.ClassMetadataConflict,
						"rotation %d from %s is not a quarter turn", s.value, s.label).
						WithContainer(c.Format).WithTrack(t.ID.Value)
				}
				final, finalLabel = normalizeRotation(s.value), s.label
				break
			}
		}
	}
	for _, s := range rotationSources {
		if s.present && normalizeRotation(s.value) != final {
			out.RotationConflicts = append(out.RotationConflicts,
				fmt.Sprintf("%s=%d", s.label, s.value))
		}
	}
	if ov.Rotation != nil {
		for _, s := range rotationSources {
			if s.present && normalizeRotation(s.value) != final {
				out.RotationConflicts = append(out.RotationConflicts,
					fmt.Sprintf("%s supplied override=%d", s.label, final))
			}
		}
	}
	out.Rotation, out.RotationFrom = final, finalLabel

	// --- crop ---
	if err := resolveCrop(c, t, ov, &out); err != nil {
		return out, err
	}

	// --- colour ---
	resolveColor(c, t, ov, &out)
	return out, nil
}

func resolveCrop(c *media.Container, t *media.Track, ov Override, out *Resolved) error {
	candidates := []struct {
		crop  *media.Crop
		label string
	}{
		{crop: ov.Crop, label: "override"},
		{crop: t.Crop, label: "track"},
	}
	var chosen *media.Crop
	chosenFrom := "default"
	for _, cand := range candidates {
		if cand.crop != nil {
			// Validate against the coded frame. A crop that leaves the frame
			// is a hard error, not a clamp, because clamping would silently
			// change the delivered picture.
			if t.Width > 0 && t.Height > 0 {
				if err := validateCrop(*cand.crop, t.Width, t.Height); err != nil {
					var e *media.Error
					if ok := asMediaError(err, &e); ok {
						return e.WithContainer(c.Format).WithTrack(t.ID.Value)
					}
					return err
				}
			}
			chosen, chosenFrom = cand.crop, cand.label
			break
		}
	}
	if chosen == nil {
		// Full frame is the documented default when no crop is declared.
		if t.Width > 0 && t.Height > 0 {
			out.Crop = &media.Crop{Width: t.Width, Height: t.Height, Source: "default"}
		}
		out.CropFrom = "default"
		return nil
	}
	finalCrop := *chosen
	finalCrop.Source = chosenFrom
	out.Crop, out.CropFrom = &finalCrop, chosenFrom
	return nil
}

func validateCrop(c media.Crop, frameW, frameH int) error {
	if c.Width <= 0 || c.Height <= 0 {
		return media.NewError(media.ClassMetadataConflict,
			"crop %dx%d has a non-positive dimension", c.Width, c.Height)
	}
	if c.X < 0 || c.Y < 0 || c.X+c.Width > frameW || c.Y+c.Height > frameH {
		return media.NewError(media.ClassMetadataConflict,
			"crop %dx%d+%d+%d leaves the %dx%d coded frame", c.Width, c.Height, c.X, c.Y, frameW, frameH)
	}
	return nil
}

func asMediaError(err error, out **media.Error) bool {
	e, ok := err.(*media.Error)
	if ok {
		*out = e
	}
	return ok
}

func resolveColor(c *media.Container, t *media.Track, ov Override, out *Resolved) {
	type candidate struct {
		color media.Color
		label string
	}
	var cands []candidate
	if ov.Color != nil {
		c := *ov.Color
		c.Present = true
		cands = append(cands, candidate{color: c, label: "override"})
	}
	if t.Color.Present {
		cands = append(cands, candidate{color: t.Color, label: "track"})
	}
	if c.Color.Present {
		cands = append(cands, candidate{color: c.Color, label: "container"})
	}

	if len(cands) == 0 {
		out.Color = media.DefaultColor
		out.ColorFrom = "default"
		out.Warnings = append(out.Warnings, "no colour description; defaulted to BT.709 limited")
		return
	}
	chosen := cands[0]
	out.Color = chosen.color
	out.Color.Source = chosen.label
	out.ColorFrom = chosen.label

	for _, other := range cands[1:] {
		if !sameColor(chosen.color, other.color) {
			out.ColorConflicts = append(out.ColorConflicts,
				fmt.Sprintf("%s=%s", other.label, other.color))
		}
	}
	// An unknown colour (all zeros, which is not a defined set of
	// characteristics) falls back to the default but keeps its origin.
	if chosen.color.Primaries == 0 && chosen.color.Transfer == 0 && chosen.color.Matrix == 0 {
		out.Warnings = append(out.Warnings,
			fmt.Sprintf("colour from %s is not a defined description; using the documented default", chosen.label))
		fallback := media.DefaultColor
		fallback.Source = chosen.label
		out.Color = fallback
	}
}

func sameColor(a, b media.Color) bool {
	return a.Primaries == b.Primaries && a.Transfer == b.Transfer &&
		a.Matrix == b.Matrix && a.FullRange == b.FullRange
}

// Lookup returns the resolution for a track key.
func (r *Result) Lookup(key string) (Resolved, bool) {
	for _, e := range r.Entries {
		if e.TrackKey == key {
			return e, true
		}
	}
	return Resolved{}, false
}
