package ingest

import "github.com/example/mediad/internal/model"

// AdjudicationPolicy selects how track- vs container-level descriptors
// are reconciled. Track wins is the safe default: a track-level edit is
// applied after the container transform in every delivery target.
type AdjudicationPolicy struct {
	// RejectContradictoryColor makes NCLC-vs-ICC conflicts fatal rather
	// than preferring the track-level description.
	RejectContradictoryColor bool
}

// DefaultPolicy mirrors the post-production convention: the most local
// (track-level) descriptor wins; only genuinely uninterpretatable
// conflicts are rejected.
func DefaultPolicy() AdjudicationPolicy {
	return AdjudicationPolicy{RejectContradictoryColor: true}
}

func validRotation(r Rotation) bool {
	switch r {
	case 0, 90, 180, 270:
		return true
	default:
		return false
	}
}

func sameCrop(a, b *Crop) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Adjudicate fills the Applied* fields of every track in the summary,
// retaining the raw descriptors and recording why each choice was made.
// Tracks whose descriptors cannot be safely interpreted are marked
// Rejected with a classified conflict error returned for them.
func Adjudicate(s *Summary, policy AdjudicationPolicy) error {
	var firstErr error
	for _, tr := range s.Tracks {
		if tr.Kind != "video" || tr.Video == nil {
			continue
		}
		if !validRotation(tr.ContainerRot) || !validRotation(tr.TrackRot) {
			tr.Rejected = true
			tr.ConflictNotes = append(tr.ConflictNotes,
				"rotation is not a multiple of 90 degrees")
			firstErr = record(firstErr, model.New(model.KindConflict, "ingest",
				"conflict", "non-orthogonal rotation cannot be applied"))
			continue
		}
		switch {
		case tr.TrackRot != 0 && tr.ContainerRot != 0 &&
			tr.TrackRot != tr.ContainerRot:
			tr.ConflictNotes = append(tr.ConflictNotes,
				"track rotation overrides contradictory container rotation")
			tr.AppliedRotation = tr.TrackRot
		case tr.TrackRot != 0:
			tr.AppliedRotation = tr.TrackRot
		default:
			tr.AppliedRotation = tr.ContainerRot
		}

		switch {
		case tr.Video.Crop != nil && tr.ContainerCrop != nil &&
			!sameCrop(tr.Video.Crop, tr.ContainerCrop):
			tr.ConflictNotes = append(tr.ConflictNotes,
				"track crop overrides contradictory container crop")
			tr.AppliedCrop = tr.Video.Crop
		case tr.Video.Crop != nil:
			tr.AppliedCrop = tr.Video.Crop
		default:
			tr.AppliedCrop = tr.ContainerCrop
		}

		tr.AppliedColor = adjudicateColor(tr, policy)
		if tr.AppliedColor == nil && len(tr.Video.Colors) > 0 {
			tr.Rejected = true
			firstErr = record(firstErr, model.New(model.KindConflict, "ingest",
				"conflict", "incompatible container/track color descriptions"))
		}
	}
	return firstErr
}

func record(prev, err error) error {
	if prev == nil {
		return err
	}
	return prev
}

// adjudicateColor returns the color to apply, or nil on hard conflict.
func adjudicateColor(tr *Track, policy AdjudicationPolicy) *Color {
	colors := tr.Video.Colors
	if len(colors) == 0 {
		return nil
	}
	// Single description: accept as-is.
	if len(colors) == 1 {
		c := colors[0]
		return &c
	}
	// Multiple descriptions must agree on the color model. Mixed NCLC
	// and ICC profiles cannot be merged without a color-managed render.
	var nclc, icc int
	for i := range colors {
		if colors[i].ICCProfile {
			icc++
		} else {
			nclc++
		}
	}
	if nclc > 0 && icc > 0 {
		tr.ConflictNotes = append(tr.ConflictNotes,
			"mixed NCLC and ICC color descriptions")
		if policy.RejectContradictoryColor {
			return nil
		}
	}
	// Same model: prefer the track-level (most local) description.
	for i := len(colors) - 1; i >= 0; i-- {
		if colors[i].Source == "track" {
			c := colors[i]
			return &c
		}
	}
	c := colors[len(colors)-1]
	return &c
}
