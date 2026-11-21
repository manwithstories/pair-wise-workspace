// Package normalize reconciles conflicting rotation, crop and colour
// descriptions so downstream stages see one coherent track.
package normalize

import (
	"fmt"

	"mediad/internal/model"
)

// ResolveColor applies the documented precedence: explicit operator override
// beats track-level description, which beats the container/format default.
// Every candidate is retained in the trace, and the count of candidates that
// disagreed with the winner is reported so the decision can be audited.
func ResolveColor(cands []model.ColorCandidate) model.ColorDecision {
	dec := model.ColorDecision{Chosen: model.DefaultColor()}
	if len(cands) == 0 {
		dec.Trace = []model.ColorCandidate{{
			Layer: model.ColorLayerContainer, Color: model.DefaultColor(), Origin: "absent",
		}}
		return dec
	}
	dec.Trace = append(dec.Trace, cands...)
	best := cands[0]
	for _, c := range cands[1:] {
		if c.Layer.Rank() > best.Layer.Rank() ||
			(c.Layer.Rank() == best.Layer.Rank() && c.Color.Description == "nclx" && best.Color.Description != "nclx") {
			best = c
		}
	}
	dec.Chosen = best.Color
	for _, c := range cands {
		if c.Layer != best.Layer || c.Origin != best.Origin {
			if !c.Color.Equal(best.Color) {
				dec.Conflicts++
			}
		}
	}
	return dec
}

// Result records what changed while normalising one track.
type Result struct {
	TrackID uint32 `json:"track_id"`
	// RotationFrom and RotationTo are degrees clockwise.
	RotationFrom int        `json:"rotation_from"`
	RotationTo   int        `json:"rotation_to"`
	Crop         model.Crop `json:"crop"`
	// ColorConflicts is copied from the arbitration.
	ColorConflicts int      `json:"color_conflicts"`
	Notes          []string `json:"notes"`
}

// Track normalises one track in place and returns a description of the
// change. Rotation outside the four right angles is invalid input; a crop
// that removes the whole frame is reported with the track ID and the track's
// time range, never silently truncated.
func Track(t *model.Track) (Result, error) {
	res := Result{TrackID: t.ID, RotationFrom: t.Rotation, RotationTo: t.Rotation, Crop: t.Crop}

	switch t.Rotation {
	case 0, 90, 180, 270:
	case -90:
		t.Rotation, res.RotationTo = 270, 270
		res.Notes = append(res.Notes, "rotation -90 normalised to 270")
	case -180, 360:
		t.Rotation, res.RotationTo = 0, 0
		res.Notes = append(res.Notes, fmt.Sprintf("rotation %d normalised to 0", res.RotationFrom))
	case -270:
		t.Rotation, res.RotationTo = 90, 90
		res.Notes = append(res.Notes, "rotation -270 normalised to 90")
	default:
		return res, model.New(model.KindInvalidMedia, "normalize",
			fmt.Sprintf("rotation %d is not a multiple of 90 degrees", t.Rotation)).WithTrack(t.ID)
	}

	if t.Kind == model.Video {
		if t.CodedWidth == 0 || t.CodedHeight == 0 {
			return res, model.New(model.KindInvalidMedia, "normalize", "video track has zero coded dimensions").WithTrack(t.ID)
		}
	} else if !t.Crop.IsZero() {
		return res, model.New(model.KindInvalidMedia, "normalize", "crop on a non-video track").WithTrack(t.ID)
	}

	cleanW, err := t.Crop.Width(t.CodedWidth)
	if err != nil {
		return res, cropError(t, err)
	}
	cleanH, err := t.Crop.Height(t.CodedHeight)
	if err != nil {
		return res, cropError(t, err)
	}
	if t.Kind == model.Video && (cleanW == 0 || cleanH == 0) {
		return res, cropError(t, fmt.Errorf("crop leaves an empty %dx%d frame", cleanW, cleanH))
	}

	dec := ResolveColor(t.ColorCandidates)
	t.Color = dec
	res.ColorConflicts = dec.Conflicts
	if dec.Conflicts > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("resolved %d conflicting colour description(s) in favour of %q",
			dec.Conflicts, dec.Chosen.Description))
	}
	if t.FrameRate.IsZero() && t.Timescale > 0 && len(t.Samples) > 1 {
		res.Notes = append(res.Notes, "frame rate derived from sample deltas")
	}
	return res, nil
}

func cropError(t *model.Track, cause error) error {
	span := "empty"
	if !t.Duration.IsZero() {
		end, err := t.End()
		if err == nil {
			span = fmt.Sprintf("[%s, %s]", t.Start.String(), end.String())
		}
	}
	e := model.Wrap(model.KindInvalidMedia, "normalize",
		fmt.Sprintf("crop out of range on %dx%d frame over time range %s: %v",
			t.CodedWidth, t.CodedHeight, span, cause), cause)
	return e.WithTrack(t.ID)
}

// Container normalises every track and returns one result per track.
func Container(c *model.Container) ([]Result, error) {
	results := make([]Result, 0, len(c.Tracks))
	for i := range c.Tracks {
		r, err := Track(&c.Tracks[i])
		if err != nil {
			return results, err
		}
		results = append(results, r)
	}
	return results, nil
}
