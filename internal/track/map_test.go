package track

import (
	"testing"

	"mkvarchive/internal/audio"
	"mkvarchive/internal/model"
	"mkvarchive/internal/video"
)

func fullTracks() []*model.Track {
	return []*model.Track{
		{UID: 1, Number: 1, Kind: model.KindVideo, HeaderOffset: 10,
			Video: &model.VideoMeta{PixelWidth: 320, PixelHeight: 240, ColorPresent: true}},
		{UID: 2, Number: 2, Kind: model.KindAudio, HeaderOffset: 40,
			Audio: &model.AudioMeta{Channels: 6, SamplingFrequency: 48000}},
		{UID: 3, Number: 3, Kind: model.KindSubtitle, HeaderOffset: 70},
	}
}

func TestBuildMappingAssignsStableIDs(t *testing.T) {
	g, err := BuildGraph(fullTracks())
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	policy := Policy{Audio: audio.PolicyRetain, Video: video.PolicyRetain}
	m1, _, err := BuildMapping(g, policy)
	if err != nil {
		t.Fatalf("mapping: %v", err)
	}
	m2, _, _ := BuildMapping(g, policy)
	if len(m1) != 3 || m1[0].ArchiveID != 1 || m1[1].ArchiveID != 2 || m1[2].ArchiveID != 3 {
		t.Fatalf("archive ids not sequential: %+v", m1)
	}
	for i := range m1 {
		if m1[i] != m2[i] {
			t.Fatalf("mapping not deterministic: %+v vs %+v", m1, m2)
		}
	}
	if m1[1].Kind != model.KindAudio || m1[2].Kind != model.KindSubtitle {
		t.Fatalf("kind mapping wrong: %+v", m1)
	}
}

func TestBuildMappingStrictPolicyRejectsConflict(t *testing.T) {
	tracks := fullTracks()
	tracks[1].Audio.SamplingFrequency = 44100
	tracks = append(tracks, &model.Track{
		UID: 4, Number: 4, Kind: model.KindAudio, HeaderOffset: 90,
		Audio: &model.AudioMeta{Channels: 2, SamplingFrequency: 48000},
	})
	g, err := BuildGraph(tracks)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	_, _, err = BuildMapping(g, Policy{}) // both policies = reject
	ae, ok := err.(*model.ArchiveError)
	if !ok || ae.Code != "AUDIO_SAMPLE_RATE_CONFLICT" {
		t.Fatalf("want strict rejection, got %v", err)
	}
}

func TestBuildMappingPropagatesWarnings(t *testing.T) {
	tracks := fullTracks()
	tracks[0].Video.ColorPresent = false // missing colour -> warning under retain
	g, err := BuildGraph(tracks)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	mappings, warnings, err := BuildMapping(g, Policy{Audio: audio.PolicyRetain, Video: video.PolicyRetain})
	if err != nil || len(mappings) != 3 {
		t.Fatalf("retain mapping failed: %v", err)
	}
	found := false
	for _, w := range warnings {
		if w.Code == "COLOR_METADATA_MISSING" {
			found = true
		}
	}
	if !found {
		t.Fatalf("metadata warning not propagated: %+v", warnings)
	}
}

func TestLookupBySourceNumber(t *testing.T) {
	g, _ := BuildGraph(fullTracks())
	mappings, _, err := BuildMapping(g, Policy{Audio: audio.PolicyRetain, Video: video.PolicyRetain})
	if err != nil {
		t.Fatalf("mapping: %v", err)
	}
	idx := LookupBySourceNumber(mappings)
	if idx[2].SourceUID != 2 || idx[3].Kind != model.KindSubtitle {
		t.Fatalf("lookup wrong: %+v", idx)
	}
}
