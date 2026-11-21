package track

import (
	"fmt"

	"mkvarchive/internal/audio"
	"mkvarchive/internal/model"
	"mkvarchive/internal/video"
)

// Mapping pairs one source track with the archive track id it will occupy.
type Mapping struct {
	SourceUID    uint64          `json:"source_track_uid"`
	SourceNumber uint64          `json:"source_track_number"`
	ArchiveID    uint64          `json:"archive_track_id"`
	Kind         model.TrackKind `json:"kind"`
	HeaderOffset int64           `json:"-"`
}

// Policy bundles the explicit retention decisions an operator must supply.
// A zero Policy rejects everything that would need an implicit transform.
type Policy struct {
	Audio audio.RetentionPolicy
	Video video.RetentionPolicy
}

// BuildMapping validates audio/video metadata under the explicit policies,
// then assigns deterministic archive track ids in dependency write order.
// Repeated runs on the same graph always produce the same id assignment.
func BuildMapping(g *Graph, policy Policy) ([]Mapping, []*model.ArchiveError, error) {
	var warnings []*model.ArchiveError

	audioTracks := g.Tracks
	if _, aw, err := audio.ValidateTracks(audioTracks, policy.Audio); err != nil {
		return nil, nil, err
	} else {
		warnings = append(warnings, aw...)
	}
	if _, vw, err := video.ValidateTracks(audioTracks, policy.Video); err != nil {
		return nil, nil, err
	} else {
		warnings = append(warnings, vw...)
	}

	order := g.DependencyOrder()
	mappings := make([]Mapping, 0, len(order))
	usedArchive := map[uint64]bool{}
	for i, uid := range order {
		tr, ok := g.ByUID(uid)
		if !ok {
			return nil, nil, model.NewError("TRACK_MAPPING_FAILED",
				fmt.Sprintf("dependency order references unknown UID %d", uid), -1)
		}
		archiveID := uint64(i + 1)
		if usedArchive[archiveID] {
			return nil, nil, model.NewError("TRACK_MAPPING_FAILED",
				"duplicate archive track id generated", tr.HeaderOffset)
		}
		usedArchive[archiveID] = true
		mappings = append(mappings, Mapping{
			SourceUID:    tr.UID,
			SourceNumber: tr.Number,
			ArchiveID:    archiveID,
			Kind:         tr.Kind,
			HeaderOffset: tr.HeaderOffset,
		})
	}
	return mappings, warnings, nil
}

// LookupBySourceNumber indexes mappings by Matroska block track number so
// the streaming writer can resolve each block in O(1).
func LookupBySourceNumber(mappings []Mapping) map[uint64]Mapping {
	out := make(map[uint64]Mapping, len(mappings))
	for _, m := range mappings {
		out[m.SourceNumber] = m
	}
	return out
}
