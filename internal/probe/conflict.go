package probe

import (
	"mediaroute/internal/fault"
)

// NormalizeRotation extracts a right-angle rotation in degrees from an
// ISOBMFF 3x3 transformation matrix (16.16 fixed point). Non right-angle
// matrices are rejected as metadata conflicts because the pipeline only
// supports orthogonal display transforms.
func NormalizeRotation(matrix [9]int32, trackID uint32) (int, *fault.Error) {
	const one = 1 << 16
	// Matrix layout is row-major 3x3; only the 2x3 display part is used.
	a, b := matrix[0], matrix[1]
	c, d := matrix[3], matrix[4]
	cells := []int32{a, b, c, d}
	orthogonal := true
	for _, v := range cells {
		if v != 0 && v != one && v != -one {
			orthogonal = false
		}
	}
	nonZero := 0
	for _, v := range cells {
		if v != 0 {
			nonZero++
		}
	}
	// The 2x2 part must be a permutation of one +1.0, one -/+1.0 and two
	// zeros with unit determinant.
	det := int64(a)*int64(d) - int64(b)*int64(c)
	if !orthogonal || nonZero != 2 || det != int64(one)*int64(one) {
		return 0, fault.New(fault.CatMetadataConflict,
			"track matrix is not an orthogonal right-angle transform").
			WithTrack(trackID)
	}
	switch {
	case a == one && d == one:
		return 0, nil
	case b == one && c == -one:
		return 90, nil
	case a == -one && d == -one:
		return 180, nil
	case b == -one && c == one:
		return 270, nil
	default:
		return 0, fault.New(fault.CatMetadataConflict,
			"unrecognised rotation matrix").WithTrack(trackID)
	}
}

// CheckCrop validates a clean-aperture rectangle against the coded frame.
// A crop that reaches outside the coded dimensions is an illegal-crop fault
// carrying the track locator.
func CheckCrop(trackID uint32, w, h uint32, crop CropRect) *fault.Error {
	if crop.Left > crop.Right || crop.Top > crop.Bottom {
		return fault.New(fault.CatIllegalCrop,
			"crop rectangle has inverted edges").WithTrack(trackID)
	}
	if crop.Right > w || crop.Bottom > h {
		return fault.New(fault.CatIllegalCrop,
			"crop rectangle %dx%d..%dx%d exceeds coded frame %dx%d",
			crop.Left, crop.Top, crop.Right, crop.Bottom, w, h).
			WithTrack(trackID)
	}
	return nil
}

// ArbitrateColour resolves a track's colour description in the mandated
// precedence order: track-level, then container-level, then the default.
// A disagreement between the track and container scopes is recorded as a
// conflict but the track-level value still wins.
func ArbitrateColour(trackID uint32, track, container *Colour) ColourDecision {
	dec := ColourDecision{TrackID: trackID, Track: track, Container: container}
	switch {
	case track != nil:
		dec.Adopted, dec.Source = *track, ScopeTrack
		if container != nil && *container != *track {
			dec.Conflict = true
		}
	case container != nil:
		dec.Adopted, dec.Source = *container, ScopeContainer
	default:
		dec.Adopted, dec.Source = DefaultColour, ScopeDefault
	}
	return dec
}

// RecordConflict appends a structured conflict record to the scan report.
func (r *ContainerReport) RecordConflict(trackID uint32, kind, detail, adopted string) {
	r.Conflicts = append(r.Conflicts, ConflictRecord{
		TrackID: trackID, Kind: kind, Detail: detail, Adopted: adopted,
	})
}
