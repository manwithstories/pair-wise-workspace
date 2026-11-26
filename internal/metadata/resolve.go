// Package metadata resolves conflicts among explicit overrides, track metadata
// and container metadata without discarding provenance.
package metadata

import (
	"fmt"

	"medialab/internal/format"
	"medialab/internal/model"
)

// Source labels used in deterministic reports.
const (
	SourceExplicit  = "explicit_override"
	SourceTrack     = "track"
	SourceContainer = "container"
)

// Override is a caller-supplied highest-priority metadata value for one track.
type Override struct {
	TrackID  uint32
	Color    *format.ColorDescription
	Rotation *int
	Crop     *format.Crop
}

// ColorDecision records the winner and every lower-priority discarded value.
type ColorDecision struct {
	Selected  *format.ColorDescription
	Priority  string
	Discarded []DiscardedColor
}

// DiscardedColor retains a losing candidate and why it was not selected.
type DiscardedColor struct {
	Color  format.ColorDescription
	Reason string
}

// ResolvedTrack is an isolated per-track adjudication result.
type ResolvedTrack struct {
	Track    *format.Track
	Valid    bool
	Rotation int
	Crop     *format.Crop
	Color    ColorDecision
	Fault    *model.Fault
}

// Report contains all track decisions and non-fatal conflict records.
type Report struct {
	Tracks []*ResolvedTrack
	Faults []*model.Fault
}

// FirstErrorOffset returns the earliest raw metadata offset with a fault.
func (r *Report) FirstErrorOffset() int64 {
	for _, f := range r.Faults {
		if f != nil {
			return f.Offset
		}
	}
	return -1
}

// Resolve adjudicates every track independently. An invalid rotation or crop
// invalidates only that track; other tracks remain available.
func Resolve(c *format.Container, overrides ...Override) *Report {
	ov := make(map[uint32]Override, len(overrides))
	for _, o := range overrides {
		ov[o.TrackID] = o
	}
	rep := &Report{}
	for _, tr := range c.Tracks {
		rt := &ResolvedTrack{Track: tr, Valid: true, Rotation: tr.Rotation, Crop: tr.Crop}
		if o, ok := ov[tr.ID]; ok {
			if o.Rotation != nil {
				rt.Rotation = *o.Rotation
			}
			if o.Crop != nil {
				rt.Crop = o.Crop
			}
		}
		rt.Color = resolveColor(c.ContainerColor, tr.Color, overrideColor(ov[tr.ID]))
		if err := validateRotation(rt.Rotation, tr); err != nil {
			rt.Valid = false
			rt.Fault = err
			rep.Faults = append(rep.Faults, err)
		} else if err := validateCrop(rt.Crop, tr); err != nil {
			rt.Valid = false
			rt.Fault = err
			rep.Faults = append(rep.Faults, err)
		}
		rep.Tracks = append(rep.Tracks, rt)
	}
	return rep
}

func overrideColor(o Override) *format.ColorDescription { return o.Color }

func resolveColor(container, track, explicit *format.ColorDescription) ColorDecision {
	d := ColorDecision{}
	switch {
	case explicit != nil:
		d.Selected = explicit
		d.Priority = SourceExplicit
		if track != nil {
			d.Discarded = append(d.Discarded, DiscardedColor{Color: *track, Reason: "track_lower_than_explicit"})
		}
		if container != nil {
			d.Discarded = append(d.Discarded, DiscardedColor{Color: *container, Reason: "container_lower_than_explicit"})
		}
	case track != nil:
		d.Selected = track
		d.Priority = SourceTrack
		if container != nil {
			d.Discarded = append(d.Discarded, DiscardedColor{Color: *container, Reason: "container_lower_than_track"})
		}
	case container != nil:
		d.Selected = container
		d.Priority = SourceContainer
	default:
		d.Priority = "none"
	}
	return d
}

func validateRotation(angle int, tr *format.Track) *model.Fault {
	switch angle {
	case 0, 90, 180, 270:
		return nil
	default:
		off := tr.RotationAt
		if off == 0 {
			off = tr.DataOffset
		}
		return model.New(model.StageMetadata, "illegal_rotation", tr.ID, off,
			fmt.Errorf("rotation %d is not one of 0/90/180/270", angle))
	}
}

func validateCrop(crop *format.Crop, tr *format.Track) *model.Fault {
	if crop == nil {
		return nil
	}
	bad := crop.Left < 0 || crop.Right < 0 || crop.Top < 0 || crop.Bottom < 0 ||
		tr.Width == 0 || tr.Height == 0 ||
		crop.Left+crop.Right >= int64(tr.Width) ||
		crop.Top+crop.Bottom >= int64(tr.Height)
	if !bad {
		return nil
	}
	return model.New(model.StageMetadata, "crop_out_of_bounds", tr.ID, crop.Offset,
		fmt.Errorf("crop margins L=%d R=%d T=%d B=%d exceed %dx%d",
			crop.Left, crop.Right, crop.Top, crop.Bottom, tr.Width, tr.Height))
}

// ValidVideoTracks returns only tracks that passed metadata isolation.
func (r *Report) ValidVideoTracks() []*ResolvedTrack {
	var out []*ResolvedTrack
	for _, t := range r.Tracks {
		if t.Valid && t.Track.Kind == format.KindVideo {
			out = append(out, t)
		}
	}
	return out
}
