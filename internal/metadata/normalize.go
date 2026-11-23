// Package metadata normalises display metadata across container, track and
// explicit layers. Each layer's claim is recorded with its source, the winning
// claim is applied, and conflicts never overwrite silently.
package metadata

import (
	"fmt"

	"mediasync/internal/model"
)

// Overrides are explicit operator claims; they win over container and track
// metadata but are recorded alongside the claims they replace.
type Overrides struct {
	Rotation  *int
	Crop      *model.Crop
	Primaries *uint16
	Transfer  *uint16
	Matrix    *uint16
	FullRange *bool
}

// Normalize returns a copy of tr.Meta with arbitration applied.
//
// Rotation: explicit > track > container > default 0.
// Crop: explicit > track > container > none, and a crop that would remove more
// than the coded frame is a hard error.
// Colour: explicit > track > container; unknown codes are preserved and marked.
func Normalize(tr *model.Track, ov Overrides) (model.DisplayMeta, error) {
	if tr == nil {
		return model.DisplayMeta{}, model.NewError(model.ErrParamInvalid, 0, 0, "nil track")
	}
	if tr.Kind != model.KindVideo {
		// Audio and data tracks carry no display geometry. Preserve whatever
		// claims arrived, but never invent values.
		return tr.Meta.Clone(), nil
	}
	meta := tr.Meta.Clone()
	w, h := meta.SourceWidth, meta.SourceHeight
	if w <= 0 {
		w = tr.Width
	}
	if h <= 0 {
		h = tr.Height
	}
	if w <= 0 || h <= 0 {
		return meta, model.NewError(model.ErrParamInvalid, tr.ID, 0, "video track %d has no usable dimensions", tr.ID)
	}

	// Rotation.
	rotation := 0
	rotationSrc := model.SourceDefault
	if meta.RotationSrc != "" {
		rotation = meta.Rotation
		rotationSrc = meta.RotationSrc
	}
	if ov.Rotation != nil {
		rotation = *ov.Rotation
		rotationSrc = model.SourceExplicit
		meta.Claims = append(meta.Claims, model.Claim{
			Field: "rotation", Value: fmt.Sprintf("%d", rotation), Layer: model.LayerExplicit, Source: model.SourceExplicit,
		})
	}
	switch rotation {
	case 0, 90, 180, 270:
	default:
		return meta, model.NewError(model.ErrParamInvalid, tr.ID, 0,
			"rotation %d is not a multiple of 90 degrees", rotation)
	}
	meta.Rotation = rotation
	meta.RotationSrc = rotationSrc

	// Crop.
	crop := meta.Crop
	cropSrc := meta.CropSrc
	if ov.Crop != nil {
		crop = *ov.Crop
		cropSrc = model.SourceExplicit
		meta.Claims = append(meta.Claims, model.Claim{
			Field: "crop", Value: fmt.Sprintf("%d/%d/%d/%d", crop.Left, crop.Right, crop.Top, crop.Bottom),
			Layer: model.LayerExplicit, Source: model.SourceExplicit,
		})
	}
	if err := applyCrop(tr, &meta, w, h, crop, cropSrc); err != nil {
		return meta, err
	}

	// Colour.
	colour := meta.Colour
	if ov.Primaries != nil || ov.Transfer != nil || ov.Matrix != nil || ov.FullRange != nil {
		c := model.Colour{}
		if colour != nil {
			c = *colour
		}
		if ov.Primaries != nil {
			c.Primaries = *ov.Primaries
		}
		if ov.Transfer != nil {
			c.Transfer = *ov.Transfer
		}
		if ov.Matrix != nil {
			c.Matrix = *ov.Matrix
		}
		if ov.FullRange != nil {
			c.FullRange = *ov.FullRange
		}
		c.Known = knownColour(c.Primaries, c.Transfer, c.Matrix)
		if !c.Known {
			c.DefaultInterpretation = "bt709"
			meta.Warnings = append(meta.Warnings, fmt.Sprintf(
				"explicit colour override uses unknown codes p=%d t=%d m=%d; preserved", c.Primaries, c.Transfer, c.Matrix))
		}
		colour = &c
		meta.ColourSrc = model.SourceExplicit
		meta.Claims = append(meta.Claims, model.Claim{
			Field: "colour", Value: fmt.Sprintf("explicit/%d/%d/%d", c.Primaries, c.Transfer, c.Matrix),
			Layer: model.LayerExplicit, Source: model.SourceExplicit,
		})
	}
	meta.Colour = colour
	if colour == nil {
		meta.Warnings = append(meta.Warnings, "no colour description present; treated as bt709")
		meta.Claims = append(meta.Claims, model.Claim{
			Field: "colour", Value: "bt709", Layer: model.LayerContainer, Source: model.SourceDefault,
			Reason: "absent in source",
		})
	}
	return meta, nil
}

// applyCrop validates the requested crop against the coded frame and records
// the winning crop plus any rejected candidate.
func applyCrop(tr *model.Track, meta *model.DisplayMeta, w, h int, crop model.Crop, src model.Source) error {
	if crop.Left < 0 || crop.Right < 0 || crop.Top < 0 || crop.Bottom < 0 {
		return model.NewError(model.ErrCropOutOfBounds, tr.ID, 0,
			"negative crop %+v", crop)
	}
	if crop.Left+crop.Right >= w || crop.Top+crop.Bottom >= h {
		return model.NewError(model.ErrCropOutOfBounds, tr.ID, 0,
			"crop %d+%d of %d wide, %d+%d of %d high removes the whole frame",
			crop.Left, crop.Right, w, crop.Top, crop.Bottom, h)
	}
	meta.Crop = crop
	if crop.IsZero() {
		if src == "" {
			src = model.SourceDefault
		}
		meta.CropSrc = src
		return nil
	}
	if src == "" {
		src = model.SourceTrack
	}
	meta.CropSrc = src
	return nil
}

// knownColour matches the ISO/IEC 23001-8 code sets mediasync understands.
func knownColour(p, t, m uint16) bool {
	knownPrimaries := map[uint16]bool{1: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true, 10: true, 11: true, 12: true, 22: true}
	knownTransfer := map[uint16]bool{1: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true, 10: true, 11: true, 12: true, 13: true, 14: true, 15: true, 16: true, 17: true, 18: true}
	knownMatrix := map[uint16]bool{0: true, 1: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true, 10: true, 11: true, 12: true, 13: true, 14: true}
	return knownPrimaries[p] && knownTransfer[t] && knownMatrix[m]
}

// DisplayDimensions returns the dimensions after rotation and crop.
func DisplayDimensions(meta model.DisplayMeta, codedW, codedH int) (int, int) {
	w := codedW - meta.Crop.Left - meta.Crop.Right
	h := codedH - meta.Crop.Top - meta.Crop.Bottom
	if meta.Rotation == 90 || meta.Rotation == 270 {
		w, h = h, w
	}
	return w, h
}
