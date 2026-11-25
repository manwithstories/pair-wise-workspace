package mapping

import (
	"testing"

	"fieldpost/internal/core"
)

func tracks() []core.Track {
	return []core.Track{
		{ID: 1, Kind: core.TrackVideo, Codec: "avc1"},
		{ID: 2, Kind: core.TrackAudio, Codec: "mp4a"},
	}
}

func TestDefaultCameraMapping(t *testing.T) {
	m, err := DefaultRules().Map("stage", tracks())
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasVideo || !m.HasAudio || m.OutputVideo != 1 || m.OutputAudio != 2 {
		t.Fatalf("mapping=%+v", m)
	}
	in := []core.Sample{{TrackID: 1, PTS: 9}, {TrackID: 2, PTS: 3}}
	out := m.RemapSamples(in)
	if out[0].TrackID != 1 || out[1].TrackID != 2 || out[0].PTS != 9 {
		t.Fatalf("remap changed timing/id: %+v", out)
	}
}

func TestMissingAndAmbiguousTracksFail(t *testing.T) {
	if _, err := DefaultRules().Map("side", []core.Track{{ID: 1, Kind: core.TrackVideo}}); err == nil {
		t.Fatal("missing audio accepted")
	}
	ambiguous := append(tracks(), core.Track{ID: 3, Kind: core.TrackVideo})
	if _, err := DefaultRules().Map("roving", ambiguous); err == nil {
		t.Fatal("ambiguous video accepted")
	}
	rules := DefaultRules()
	r := rules["roving"]
	r.InputVideoID = 3
	rules["roving"] = r
	m, err := rules.Map("roving", ambiguous)
	if err != nil || m.Video.ID != 3 {
		t.Fatalf("explicit disambiguation failed: %v", err)
	}
}
