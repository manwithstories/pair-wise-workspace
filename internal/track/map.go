// Package track applies explicit, auditable audio/video mapping rules. No
// track is ever selected implicitly: every non-hint track in the container
// must appear in the explicit mapping and duplicated assignments are rejected
// with a locatable model.Error.
package track

import (
	"fmt"
	"sort"

	"mediaarchive/internal/demux"
	"mediaarchive/internal/model"
)

// Role constants describe the archive role a track is mapped to.
const (
	RoleVideo = "video"
	RoleAudio = "audio"
)

// Assignment is one explicit rule: container track TrackID takes Role.
type Assignment struct {
	TrackID uint32
	Role    string
}

// Mapping is the validated result. IndexedByTrack maps container ids to
// roles; OrderedTracks lists mapped track ids in role then id order so every
// later stage stays deterministic.
type Mapping struct {
	IndexedByTrack map[uint32]string
	OrderedTracks  []uint32
	VideoIDs       []uint32
	AudioIDs       []uint32
}

// Build validates the explicit assignments against the container.
//
// Rules:
//  1. every assignment must reference an existing track and match its kind
//     (a video rule cannot point at an audio track, and vice versa);
//  2. no track id may be assigned twice;
//  3. every non-hint track declared by the container must be mapped — an
//     unmapped track is an error, never a silent skip;
//  4. at least one track must be mapped.
func Build(c *demux.Container, assignments []Assignment) (*Mapping, error) {
	m := &Mapping{IndexedByTrack: map[uint32]string{}}
	seen := map[uint32]bool{}
	for _, a := range assignments {
		if a.Role != RoleVideo && a.Role != RoleAudio {
			return nil, model.NewError(model.StageTrack,
				fmt.Sprintf("unknown mapping role %q for track %d", a.Role, a.TrackID)).
				WithTrack(int32(a.TrackID))
		}
		if seen[a.TrackID] {
			return nil, model.NewError(model.StageTrack,
				fmt.Sprintf("duplicated mapping entry for track %d", a.TrackID)).
				WithTrack(int32(a.TrackID))
		}
		seen[a.TrackID] = true
		tm, ok := c.Track(a.TrackID)
		if !ok {
			return nil, model.NewError(model.StageTrack,
				fmt.Sprintf("mapping references non-existent track %d", a.TrackID)).
				WithTrack(int32(a.TrackID))
		}
		want := demux.KindVideo
		if a.Role == RoleAudio {
			want = demux.KindAudio
		}
		if tm.Kind != want {
			return nil, model.NewError(model.StageTrack,
				fmt.Sprintf("track %d is %s but mapped as %s", a.TrackID, tm.Kind, a.Role)).
				WithTrack(int32(a.TrackID))
		}
		m.IndexedByTrack[a.TrackID] = a.Role
		if a.Role == RoleVideo {
			m.VideoIDs = append(m.VideoIDs, a.TrackID)
		} else {
			m.AudioIDs = append(m.AudioIDs, a.TrackID)
		}
	}
	var unmapped []uint32
	for _, tm := range c.Tracks {
		if tm.Kind == demux.KindHint {
			continue // hint tracks are processed by the RTP stage, not mapped
		}
		if !seen[tm.ID] {
			unmapped = append(unmapped, tm.ID)
		}
	}
	if len(unmapped) > 0 {
		sort.Slice(unmapped, func(i, j int) bool { return unmapped[i] < unmapped[j] })
		return nil, model.NewError(model.StageTrack,
			fmt.Sprintf("unmapped tracks require explicit rules: %v", unmapped)).
			WithTrack(int32(unmapped[0]))
	}
	if len(m.IndexedByTrack) == 0 {
		return nil, model.NewError(model.StageTrack, "empty track mapping: at least one rule required")
	}
	sort.Slice(m.VideoIDs, func(i, j int) bool { return m.VideoIDs[i] < m.VideoIDs[j] })
	sort.Slice(m.AudioIDs, func(i, j int) bool { return m.AudioIDs[i] < m.AudioIDs[j] })
	m.OrderedTracks = append(append([]uint32{}, m.VideoIDs...), m.AudioIDs...)
	return m, nil
}

// IsMapped reports whether a track participates in the archive output.
func (m *Mapping) IsMapped(trackID uint32) bool {
	_, ok := m.IndexedByTrack[trackID]
	return ok
}
