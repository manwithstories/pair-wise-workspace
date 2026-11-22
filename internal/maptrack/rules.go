// Package maptrack applies the manifest's track mapping rules to a demuxed
// asset. It enforces the "one video, one audio, N named auxiliary" delivery
// profile, and rejects an asset when a required slot is missing or two slots
// resolve to the same source track. Rejection is per asset: the caller keeps
// the progress of other assets.
package maptrack

import (
	"fmt"
	"sort"

	"mediabridge/internal/fault"
	"mediabridge/internal/media"
)

// Rule is the resolved mapping from manifest terms onto track selectors.
type Rule struct {
	// VideoTrackID pins the video track; 0 = first video track.
	VideoTrackID uint32
	// AudioTrackID pins the audio track; 0 = first audio track.
	AudioTrackID uint32
	// Auxiliary declares the auxiliary slots in delivery order.
	Auxiliary []AuxSlot
	// DropUnmapped discards tracks that are not mapped. When false, an
	// unmapped non-trivial track is a fatal error.
	DropUnmapped bool
}

// AuxSlot names one auxiliary delivery slot.
type AuxSlot struct {
	Name string
	// TrackID selects an MP4 auxiliary track by id.
	TrackID uint32
	// Source selects an RTP auxiliary track by its manifest path. Auxiliary
	// RTP tracks are passed to the mapper as pre-tagged tracks keyed by this
	// string.
	Source string
}

// Assignment is one output track of the delivery profile.
type Assignment struct {
	// Slot is "video", "audio", or the auxiliary slot name.
	Slot string
	// TrackIndex is the source track index in the demuxed asset.
	TrackIndex int
	// TrackID is the source track id (MP4 track_ID or RTP SSRC).
	TrackID uint32
	// Kind is the source track kind.
	Kind media.TrackKind
}

// Plan is the result of applying rules to an asset.
type Plan struct {
	Assignments []Assignment
	// Dropped lists source tracks not carried into the output.
	Dropped []int
	// Warnings are non-fatal mapping observations.
	Warnings []string
}

// AuxTrack is a pre-demuxed auxiliary track supplied by the caller (RTP).
type AuxTrack struct {
	// Source is the manifest path the dump came from.
	Source string
	// Index is the index this track will occupy in the merged asset.
	Index int
	// ID is the logical track id (SSRC or override).
	ID uint32
	// Kind is always media.KindAux.
	Kind media.TrackKind
}

// Apply resolves rules against the asset's tracks. aux lists auxiliary tracks
// that are already merged into asset (their TrackIndex values must exist).
func Apply(rules Rule, asset *media.Asset, aux []AuxTrack) (*Plan, error) {
	if asset == nil || len(asset.Tracks) == 0 {
		return nil, fault.New(fault.CodeMapping, fault.Location{AssetID: assetID(asset)},
			"asset has no tracks").WithOp("maptrack.Apply")
	}
	plan := &Plan{}
	used := map[int]string{}

	// Video slot.
	video, err := selectTrack(asset, media.KindVideo, rules.VideoTrackID, "video")
	if err != nil {
		return nil, err
	}
	plan.Assignments = append(plan.Assignments, Assignment{
		Slot: "video", TrackIndex: video.Index, TrackID: video.ID, Kind: video.Kind,
	})
	used[video.Index] = "video"

	// Audio slot.
	audio, err := selectTrack(asset, media.KindAudio, rules.AudioTrackID, "audio")
	if err != nil {
		return nil, err
	}
	plan.Assignments = append(plan.Assignments, Assignment{
		Slot: "audio", TrackIndex: audio.Index, TrackID: audio.ID, Kind: audio.Kind,
	})
	used[audio.Index] = "audio"

	// Auxiliary slots.
	auxBySource := map[string]AuxTrack{}
	for _, a := range aux {
		auxBySource[a.Source] = a
	}
	for _, slot := range rules.Auxiliary {
		var idx int
		var id uint32
		switch {
		case slot.Source != "":
			a, ok := auxBySource[slot.Source]
			if !ok {
				return nil, fault.New(fault.CodeMissingMapping,
					fault.Location{AssetID: asset.ID, SourcePath: slot.Source},
					"auxiliary slot %q: no RTP track for source", slot.Name).
					WithOp("maptrack.Apply")
			}
			if prev, dup := used[a.Index]; dup {
				return nil, fault.New(fault.CodeDuplicateMapping,
					fault.Location{AssetID: asset.ID, SourcePath: slot.Source}.WithTrack(a.ID),
					"auxiliary slot %q reuses track already mapped as %q", slot.Name, prev).
					WithOp("maptrack.Apply")
			}
			idx, id = a.Index, a.ID
		default:
			t, ok := asset.TrackByID(slot.TrackID)
			if !ok {
				return nil, fault.New(fault.CodeMissingMapping,
					fault.Location{AssetID: asset.ID}.WithTrack(slot.TrackID),
					"auxiliary slot %q: no MP4 track %d", slot.Name, slot.TrackID).
					WithOp("maptrack.Apply")
			}
			if t.Kind != media.KindAux {
				return nil, fault.New(fault.CodeMapping,
					fault.Location{AssetID: asset.ID}.WithTrack(slot.TrackID),
					"auxiliary slot %q requires aux track, track %d is %s",
					slot.Name, slot.TrackID, t.Kind).WithOp("maptrack.Apply")
			}
			if prev, dup := used[t.Index]; dup {
				return nil, fault.New(fault.CodeDuplicateMapping,
					fault.Location{AssetID: asset.ID}.WithTrack(trackIDOrZero(t.ID)),
					"auxiliary slot %q reuses track already mapped as %q", slot.Name, prev).
					WithOp("maptrack.Apply")
			}
			idx, id = t.Index, t.ID
		}
		if _, dup := used[idx]; dup {
			return nil, fault.New(fault.CodeDuplicateMapping,
				fault.Location{AssetID: asset.ID}.WithTrack(id),
				"auxiliary slot %q maps a duplicate track index %d", slot.Name, idx).
				WithOp("maptrack.Apply")
		}
		used[idx] = slot.Name
		plan.Assignments = append(plan.Assignments, Assignment{
			Slot: slot.Name, TrackIndex: idx, TrackID: id, Kind: media.KindAux,
		})
	}

	// Unmapped tracks.
	for _, t := range asset.Tracks {
		if _, ok := used[t.Index]; ok {
			continue
		}
		if rules.DropUnmapped {
			plan.Dropped = append(plan.Dropped, t.Index)
			plan.Warnings = append(plan.Warnings,
				fmt.Sprintf("dropped unmapped track id=%d kind=%s", t.ID, t.Kind))
			continue
		}
		return nil, fault.New(fault.CodeMissingMapping,
			fault.Location{AssetID: asset.ID}.WithTrack(t.ID),
			"track id=%d kind=%s is not mapped and drop_unmapped is false", t.ID, t.Kind).
			WithOp("maptrack.Apply").
			WithRecovery("add an auxiliary slot or set drop_unmapped")
	}

	sort.SliceStable(plan.Assignments, func(i, j int) bool {
		return plan.Assignments[i].TrackIndex < plan.Assignments[j].TrackIndex
	})
	return plan, nil
}

func selectTrack(asset *media.Asset, kind media.TrackKind, pinned uint32, slot string) (media.Track, error) {
	if pinned != 0 {
		t, ok := asset.TrackByID(pinned)
		if !ok {
			return media.Track{}, fault.New(fault.CodeMissingMapping,
				fault.Location{AssetID: asset.ID}.WithTrack(pinned),
				"%s slot pinned to missing track %d", slot, pinned).
				WithOp("maptrack.Apply")
		}
		if t.Kind != kind {
			return media.Track{}, fault.New(fault.CodeMapping,
				fault.Location{AssetID: asset.ID}.WithTrack(pinned),
				"%s slot pinned to track %d of kind %s", slot, pinned, t.Kind).
				WithOp("maptrack.Apply")
		}
		return t, nil
	}
	for _, t := range asset.Tracks {
		if t.Kind == kind {
			return t, nil
		}
	}
	return media.Track{}, fault.New(fault.CodeMissingMapping,
		fault.Location{AssetID: asset.ID},
		"asset has no %s track", slot).WithOp("maptrack.Apply")
}

func assetID(a *media.Asset) string {
	if a == nil {
		return ""
	}
	return a.ID
}

func trackIDOrZero(id uint32) uint32 { return id }
