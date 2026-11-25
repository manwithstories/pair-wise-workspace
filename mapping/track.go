// Package mapping applies camera-specific essence track rules without decoding.
package mapping

import (
	"sort"

	"fieldpost/internal/core"
)

// Rule selects required input tracks and assigns stable output track IDs.
// A zero Input*ID means there must be exactly one track of that kind.
type Rule struct {
	Camera       string
	InputVideoID uint32
	InputAudioID uint32
	OutputVideo  uint32
	OutputAudio  uint32
	RequireVideo bool
	RequireAudio bool
}

// Mapping is the resolved track map for one camera/shard.
type Mapping struct {
	Camera      string
	Video       core.Track
	Audio       core.Track
	HasVideo    bool
	HasAudio    bool
	OutputVideo uint32
	OutputAudio uint32
}

// Rules is a camera rule table.
type Rules map[string]Rule

// DefaultRules returns deterministic rules for stage, side and roving cameras.
// All three use one AVC and one AAC track; explicit IDs can disambiguate a
// camera that carries duplicate essences.
func DefaultRules() Rules {
	return Rules{
		"stage":  {Camera: "stage", OutputVideo: 1, OutputAudio: 2, RequireVideo: true, RequireAudio: true},
		"side":   {Camera: "side", OutputVideo: 1, OutputAudio: 2, RequireVideo: true, RequireAudio: true},
		"roving": {Camera: "roving", OutputVideo: 1, OutputAudio: 2, RequireVideo: true, RequireAudio: true},
		"*":      {Camera: "*", OutputVideo: 1, OutputAudio: 2, RequireVideo: true, RequireAudio: true},
	}
}

// Map resolves tracks for camera. It never chooses silently when a required
// kind is ambiguous and the camera rule does not name an input ID.
func (rs Rules) Map(camera string, tracks []core.Track) (*Mapping, error) {
	rule, ok := rs[camera]
	if !ok {
		rule, ok = rs["*"]
		if !ok {
			return nil, core.New(core.StageMapping, core.ExitUsage, false,
				"no track mapping rule for camera %q", camera)
		}
	}
	out := &Mapping{Camera: camera, OutputVideo: rule.OutputVideo, OutputAudio: rule.OutputAudio}
	if out.OutputVideo == 0 {
		out.OutputVideo = 1
	}
	if out.OutputAudio == 0 {
		out.OutputAudio = 2
	}

	videos := byKind(tracks, core.TrackVideo)
	audios := byKind(tracks, core.TrackAudio)

	if rule.RequireVideo {
		tr, err := selectTrack(camera, core.TrackVideo, videos, rule.InputVideoID)
		if err != nil {
			return nil, err
		}
		out.Video, out.HasVideo = tr, true
	}
	if rule.RequireAudio {
		tr, err := selectTrack(camera, core.TrackAudio, audios, rule.InputAudioID)
		if err != nil {
			return nil, err
		}
		out.Audio, out.HasAudio = tr, true
	}
	return out, nil
}

func byKind(tracks []core.Track, kind core.TrackKind) []core.Track {
	var out []core.Track
	for _, tr := range tracks {
		if tr.Kind == kind {
			out = append(out, tr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func selectTrack(camera string, kind core.TrackKind, tracks []core.Track, explicitID uint32) (core.Track, error) {
	if explicitID != 0 {
		for _, tr := range tracks {
			if tr.ID == explicitID {
				return tr, nil
			}
		}
		return core.Track{}, core.New(core.StageMapping, core.ExitUsage, false,
			"camera %q requires %s track id %d but it is missing", camera, kind, explicitID)
	}
	if len(tracks) == 0 {
		return core.Track{}, core.New(core.StageMapping, core.ExitUsage, false,
			"camera %q is missing required %s track", camera, kind)
	}
	if len(tracks) > 1 {
		ids := make([]uint32, 0, len(tracks))
		for _, tr := range tracks {
			ids = append(ids, tr.ID)
		}
		return core.Track{}, core.New(core.StageMapping, core.ExitUsage, false,
			"camera %q has ambiguous %s tracks ids=%v; rule must name input id", camera, kind, ids)
	}
	return tracks[0], nil
}

// RemapSamples copies samples and rewrites their track IDs to output IDs.
// Essence bytes and timing are untouched.
func (m *Mapping) RemapSamples(in []core.Sample) []core.Sample {
	out := make([]core.Sample, len(in))
	copy(out, in)
	for i := range out {
		switch out[i].TrackID {
		case m.Video.ID:
			out[i].TrackID = m.OutputVideo
		case m.Audio.ID:
			out[i].TrackID = m.OutputAudio
		}
	}
	return out
}
