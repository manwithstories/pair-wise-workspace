package audio

import (
	"testing"

	"media-check/internal/model"
)

func TestNormalizeAliasAndUndeclared(t *testing.T) {
	res := Adapter{}.Normalize([]Meta{
		{Channels: 2, Layout: "2.0", SampleRate: 48000},
		{Channels: 2, Layout: "", SampleRate: 48000},
	})
	if res.Isolated[0] || res.Isolated[1] {
		t.Fatalf("lossless cases must not isolate: %v", res.Isolated)
	}
	if res.Normalized[0].Layout != "stereo" || res.Normalized[1].Layout != "stereo" {
		t.Fatalf("normalisation failed: %+v", res.Normalized)
	}
	var actions []string
	for _, d := range res.Decisions {
		actions = append(actions, d.Action)
	}
	if len(actions) != 2 || actions[0] != "normalise-layout" || actions[1] != "fill-layout" {
		t.Fatalf("decisions not traceable: %v", actions)
	}
}

func TestNormalizeIsolatesUnmappable(t *testing.T) {
	metas := []Meta{
		{Channels: 2, Layout: "stereo", SampleRate: 48000},
		{Channels: 6, Layout: "5.1", SampleRate: 48000},    // layout unmappable
		{Channels: 2, Layout: "stereo", SampleRate: 44100}, // rate unmappable
		{Channels: 2, Layout: "stereo", SampleRate: 48000},
	}
	res := Adapter{}.Normalize(metas)
	if !res.Isolated[1] || !res.Isolated[2] || res.Isolated[3] {
		t.Fatalf("isolation wrong: %v", res.Isolated)
	}
	if !res.Faults.HasCode(model.FaultAudioIsolated) {
		t.Fatal("expected audio_isolated faults")
	}
	// original metadata preserved in the decision log
	var kept bool
	for _, d := range res.Decisions {
		if d.Segment == 1 && d.Action == "isolate" &&
			d.Original.Layout == "5.1" && d.Original.Channels == 6 {
			kept = true
		}
	}
	if !kept {
		t.Fatal("isolated segment lost its original metadata")
	}
	if res.Normalized[3].Layout != "stereo" {
		t.Fatal("later valid segments must still normalise")
	}
}

func TestNormalizeLayoutChannelConflict(t *testing.T) {
	res := Adapter{}.Normalize([]Meta{
		{Channels: 2, Layout: "stereo", SampleRate: 48000},
		{Channels: 4, Layout: "stereo", SampleRate: 48000}, // contradicts channels
	})
	if !res.Isolated[1] {
		t.Fatal("layout/channel conflict must isolate")
	}
}
