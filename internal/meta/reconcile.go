package meta

import (
	"fmt"

	"relaycheck/internal/media"
)

// DefaultLayer returns the explicit, adjudicated fallback values. These are
// deliberately concrete rather than guessed.
func DefaultLayer() Layer {
	return Layer{
		Rotation: &Rotation{Degrees: 0},
		Color: &Color{
			ColourType: "nclx",
			Primaries:  "bt709",
			Transfer:   "bt709",
			Matrix:     "bt709",
			Known:      true,
		},
	}
}

type attrClaim[T comparable] struct {
	source Source
	value  T
	set    bool
}

// resolveAttr applies override > track > container > default and records one
// conflict when two present sources disagree. Claims must be supplied highest
// precedence first.
func resolveAttr[T comparable](trackID int64, name string, claims []attrClaim[T], out *[]Conflict) (T, Source, bool) {
	var zero T
	present := make([]attrClaim[T], 0, len(claims))
	for _, c := range claims {
		if c.set {
			present = append(present, c)
		}
	}
	if len(present) == 0 {
		return zero, SourceDefault, false
	}
	chosen := present[0].value
	chosenSrc := present[0].source

	allEqual := true
	for _, c := range present[1:] {
		if c.value != chosen {
			allEqual = false
			break
		}
	}
	if !allEqual {
		sources := make([]string, len(present))
		values := make([]string, len(present))
		for i, c := range present {
			sources[i] = c.source.String()
			values[i] = fmt.Sprintf("%v", c.value)
		}
		*out = append(*out, Conflict{
			TrackID:   trackID,
			Attribute: name,
			Sources:   sources,
			Values:    values,
			Chosen:    chosenSrc.String(),
			Message:   fmt.Sprintf("%s disagreement on track %d; selected %s", name, trackID, chosenSrc),
		})
	}
	return chosen, chosenSrc, true
}

// ReconcileInput carries the four precedence tiers for one track.
type ReconcileInput struct {
	TrackID       int64
	Type          string
	TimeScale     uint32
	Duration      int64
	Width, Height uint32
	Override      Layer
	Track         Layer
	Container     Layer
	Default       Layer
}

// ReconcileTrack adjudicates one track and validates the selected values. A
// fault, when returned, already carries the track id and exact time range.
func ReconcileTrack(in ReconcileInput) (ResolvedTrack, *media.Fault) {
	conflicts := []Conflict{}

	tr := ResolvedTrack{
		ID: in.TrackID, Type: in.Type,
		TimeScale: in.TimeScale, Duration: in.Duration,
		Width: in.Width, Height: in.Height,
	}

	if in.TimeScale == 0 {
		f := media.NewFault(media.CodeInvalidTimebase, "track %d has zero timescale", in.TrackID).
			WithTrack(in.TrackID)
		return tr, f
	}
	start, f := media.Rat(0, 1)
	if f != nil {
		return tr, f.WithTrack(in.TrackID)
	}
	end, f := media.Rat(in.Duration, int64(in.TimeScale))
	if f != nil {
		return tr, f.WithTrack(in.TrackID)
	}
	tr.TimeRange = media.TimeRange{Start: start, End: end}

	rot, _, hasRot := resolveAttr(in.TrackID, "rotation", []attrClaim[Rotation]{
		{source: SourceOverride, value: derefRot(in.Override.Rotation), set: in.Override.Rotation != nil},
		{source: SourceTrack, value: derefRot(in.Track.Rotation), set: in.Track.Rotation != nil},
		{source: SourceContainer, value: derefRot(in.Container.Rotation), set: in.Container.Rotation != nil},
		{source: SourceDefault, value: derefRot(in.Default.Rotation), set: in.Default.Rotation != nil},
	}, &conflicts)
	if hasRot {
		tr.Rotation = rot
		if !rot.Legal() {
			return tr, media.NewFault(media.CodeInvalidRotation, "illegal rotation angle %d", rot.Degrees).
				WithTrack(in.TrackID).WithRange(tr.TimeRange.Start, tr.TimeRange.End)
		}
	}

	crop, _, hasCrop := resolveAttr(in.TrackID, "crop", []attrClaim[Crop]{
		{source: SourceOverride, value: derefCrop(in.Override.Crop), set: in.Override.Crop != nil},
		{source: SourceTrack, value: derefCrop(in.Track.Crop), set: in.Track.Crop != nil},
		{source: SourceContainer, value: derefCrop(in.Container.Crop), set: in.Container.Crop != nil},
		{source: SourceDefault, value: derefCrop(in.Default.Crop), set: in.Default.Crop != nil},
	}, &conflicts)
	if hasCrop {
		c := crop
		tr.Crop = &c
		if !crop.InBounds() {
			return tr, media.NewFault(media.CodeInvalidCrop, "crop rectangle %+v out of bounds", crop).
				WithTrack(in.TrackID).WithRange(tr.TimeRange.Start, tr.TimeRange.End)
		}
	}

	col, _, hasColor := resolveAttr(in.TrackID, "color", []attrClaim[Color]{
		{source: SourceOverride, value: derefColor(in.Override.Color), set: in.Override.Color != nil},
		{source: SourceTrack, value: derefColor(in.Track.Color), set: in.Track.Color != nil},
		{source: SourceContainer, value: derefColor(in.Container.Color), set: in.Container.Color != nil},
		{source: SourceDefault, value: derefColor(in.Default.Color), set: in.Default.Color != nil},
	}, &conflicts)
	if hasColor {
		c := col
		tr.Color = &c
		if !col.Known {
			return tr, media.NewFault(media.CodeUnknownColor, "unknown color marker %d (%s)", col.RawMarker, col.ColourType).
				WithTrack(in.TrackID).WithRange(tr.TimeRange.Start, tr.TimeRange.End)
		}
	}

	tr.Conflicts = conflicts
	return tr, nil
}

func derefRot(p *Rotation) Rotation {
	if p == nil {
		return Rotation{}
	}
	return *p
}
func derefCrop(p *Crop) Crop {
	if p == nil {
		return Crop{}
	}
	return *p
}
func derefColor(p *Color) Color {
	if p == nil {
		return Color{}
	}
	return *p
}
