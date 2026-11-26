// Package pipeline turns resolved metadata into normalized geometry.
package pipeline

import (
	"medialab/internal/metadata"
	"medialab/internal/model"
)

// Rect is an integer pixel rectangle after crop normalization.
type Rect struct {
	X int64
	Y int64
	W int64
	H int64
}

// Transform is a deterministic normalized crop/rotation operation.
type Transform struct {
	TrackID  uint32
	Valid    bool
	SourceW  uint32
	SourceH  uint32
	Crop     Rect
	Rotation int
	OutputW  int64
	OutputH  int64
	State    model.RecoveryState
	Origin   *model.Fault
}

// Result keeps valid transforms separate from isolated bad tracks.
type Result struct {
	Transforms []*Transform
	Rejected   []*Transform
}

// Build normalizes every resolved track. Invalid metadata is propagated with
// its original byte offset rather than being flattened into a generic error.
func Build(report *metadata.Report) *Result {
	out := &Result{}
	for _, rt := range report.Tracks {
		tr := normalizeOne(rt)
		if tr.Valid {
			out.Transforms = append(out.Transforms, tr)
		} else {
			out.Rejected = append(out.Rejected, tr)
		}
	}
	return out
}

func normalizeOne(rt *metadata.ResolvedTrack) *Transform {
	t := rt.Track
	tr := &Transform{
		TrackID:  t.ID,
		SourceW:  t.Width,
		SourceH:  t.Height,
		Rotation: rt.Rotation,
		State:    model.StateOriginal,
	}
	if !rt.Valid {
		f := rt.Fault
		if f != nil {
			f = f.At(model.StageTransform)
		}
		tr.Valid = false
		tr.State = model.StateRejected
		tr.Origin = f
		return tr
	}
	w := int64(t.Width)
	h := int64(t.Height)
	crop := Rect{X: 0, Y: 0, W: w, H: h}
	if rt.Crop != nil {
		c := rt.Crop
		crop = Rect{X: c.Left, Y: c.Top, W: w - c.Left - c.Right, H: h - c.Top - c.Bottom}
	}
	tr.Crop = crop
	if rt.Rotation == 90 || rt.Rotation == 270 {
		tr.OutputW, tr.OutputH = crop.H, crop.W
	} else {
		tr.OutputW, tr.OutputH = crop.W, crop.H
	}
	tr.Valid = crop.W > 0 && crop.H > 0 && tr.OutputW > 0 && tr.OutputH > 0
	if !tr.Valid {
		f := model.New(model.StageTransform, "normalized_empty_rect", t.ID, cropOffset(rt), nil)
		tr.State = model.StateRejected
		tr.Origin = f
	}
	return tr
}

func cropOffset(rt *metadata.ResolvedTrack) int64 {
	if rt.Crop != nil {
		return rt.Crop.Offset
	}
	return rt.Track.DataOffset
}

// FirstErrorOffset returns the first rejected transform's origin offset.
func (r *Result) FirstErrorOffset() int64 {
	for _, tr := range r.Rejected {
		if tr.Origin != nil {
			return tr.Origin.Offset
		}
	}
	return -1
}
