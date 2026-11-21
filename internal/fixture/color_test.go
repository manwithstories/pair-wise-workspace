package fixture

import (
	"testing"

	"mediad/internal/normalize"
	"mediad/internal/probe"
)

func TestConflictingColorIsRecorded(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/conflict.mp4"
	spec := MP4Spec{Tracks: []TrackSpec{{
		ID: 4, Kind: "video", Codec: "avc1", Timescale: 30000,
		Frames: 5, FrameDelta: 1000, Width: 320, Height: 240, SampleBytes: 16,
		Color:         &ColorSpec{Primaries: 1, Transfer: 1, Matrix: 1},
		ConflictColor: &ColorSpec{Primaries: 9, Transfer: 16, Matrix: 9},
	}}}
	if _, err := WriteMP4(path, spec, 5); err != nil {
		t.Fatal(err)
	}
	c, err := probe.MP4(path, probe.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	if len(tr.ColorCandidates) != 2 {
		t.Fatalf("candidates=%d want 2", len(tr.ColorCandidates))
	}
	r, err := normalize.Track(&tr)
	if err != nil {
		t.Fatal(err)
	}
	if r.ColorConflicts != 1 {
		t.Fatalf("conflicts=%d want 1", r.ColorConflicts)
	}
	if tr.Color.Chosen.Primaries != 1 {
		t.Fatalf("winner primaries=%d", tr.Color.Chosen.Primaries)
	}
	if len(tr.Color.Trace) != 2 {
		t.Fatalf("trace=%d", len(tr.Color.Trace))
	}
}

func TestAbsentColorRecordsDefault(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/nocolor.mp4"
	spec := MP4Spec{Tracks: []TrackSpec{{
		ID: 5, Kind: "video", Codec: "avc1", Timescale: 30000,
		Frames: 3, FrameDelta: 1000, Width: 320, Height: 240, SampleBytes: 8,
	}}}
	if _, err := WriteMP4(path, spec, 5); err != nil {
		t.Fatal(err)
	}
	c, err := probe.MP4(path, probe.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	if _, err := normalize.Track(&tr); err != nil {
		t.Fatal(err)
	}
	if tr.Color.Chosen.Primaries != 2 || tr.Color.Chosen.Description != "default" {
		t.Fatalf("absent colour=%+v", tr.Color.Chosen)
	}
	if len(tr.Color.Trace) != 1 || tr.Color.Trace[0].Origin != "absent" {
		t.Fatalf("trace=%+v", tr.Color.Trace)
	}
}
