package fixture

import (
	"os"
	"testing"

	"mediad/internal/clock"
	"mediad/internal/model"
	"mediad/internal/normalize"
	"mediad/internal/probe"
)

func videoSpec() MP4Spec {
	return MP4Spec{Tracks: []TrackSpec{{
		ID: 1, Kind: "video", Codec: "avc1", Timescale: 30000,
		Frames: 60, FrameDelta: 1001, Width: 1920, Height: 1080,
		Rotation: 90, CropLeft: 4, CropRight: 4, SampleBytes: 512, GOPLength: 30,
		Color: &ColorSpec{Primaries: 1, Transfer: 1, Matrix: 1},
	}}}
}

func TestMP4RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/sample.mp4"
	if _, err := WriteMP4(path, videoSpec(), 17); err != nil {
		t.Fatal(err)
	}
	c, err := probe.MP4(path, probe.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tracks) != 1 {
		t.Fatalf("tracks=%d", len(c.Tracks))
	}
	tr := c.Tracks[0]
	if tr.ID != 1 || tr.Codec != "avc1" || tr.Kind != model.Video {
		t.Fatalf("track=%+v", tr)
	}
	if tr.Rotation != 270 {
		// A counter-clockwise 90-degree tkhd matrix is a clockwise 270.
		t.Fatalf("rotation=%d want 270", tr.Rotation)
	}
	if tr.Crop.Left != 4 || tr.Crop.Right != 4 {
		t.Fatalf("crop=%+v", tr.Crop)
	}
	if tr.DisplayWidth() != 1080 || tr.DisplayHeight() != 1912 {
		t.Fatalf("display=%dx%d", tr.DisplayWidth(), tr.DisplayHeight())
	}
	if want := clock.MustNew(1001, 30000); tr.Samples[0].Duration != want {
		t.Fatalf("frame duration=%v want %v", tr.Samples[0].Duration, want)
	}
	if got := tr.FrameRate; got != clock.MustNew(30000, 1001) {
		t.Fatalf("frame rate=%v", got)
	}
	if len(tr.Samples) != 60 {
		t.Fatalf("samples=%d", len(tr.Samples))
	}
	// GOP 30 on 60 frames: samples 0 and 29 are sync samples (stss is
	// 1-based, so the wire numbers are 1 and 59... i.e. 1 and 31).
	if len(tr.Samples) != 60 {
		t.Fatalf("samples=%d", len(tr.Samples))
	}
	keyCount := 0
	for _, s := range tr.Samples {
		if s.Keyframe {
			keyCount++
		}
	}
	if keyCount != 2 {
		t.Fatalf("keyframes=%d want 2 (GOP 30 over 60 samples)", keyCount)
	}
	if !tr.Samples[0].Keyframe || !tr.Samples[30].Keyframe {
		t.Fatalf("keyframes should be samples 1 and 31 (1-based)")
	}
}

func TestMP4Deterministic(t *testing.T) {
	dir := t.TempDir()
	a, b := dir+"/a.mp4", dir+"/b.mp4"
	if _, err := WriteMP4(a, videoSpec(), 17); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteMP4(b, videoSpec(), 17); err != nil {
		t.Fatal(err)
	}
	da, _ := readAll(a)
	db, _ := readAll(b)
	if string(da) != string(db) {
		t.Fatal("same seed produced different bytes")
	}
}

func TestMXFMatchesMP4(t *testing.T) {
	dir := t.TempDir()
	mp4Path := dir + "/sample.mp4"
	mxfPath := dir + "/sample.mxf"
	if _, err := WriteMP4(mp4Path, videoSpec(), 17); err != nil {
		t.Fatal(err)
	}
	mxfSpec := MXFSpec{Name: "studio cam", Tracks: []MXFTrack{{
		TrackID: 1, Video: true, EditRateN: 30000, EditRateD: 1001,
		EditUnits: 60, StoredWidth: 1920, StoredHeight: 1080,
		Rotation: 270, HorizCrop: 4, Color: &ColorSpec{Primaries: 1, Transfer: 1, Matrix: 1},
		EssenceBytes: 512,
	}}}
	if _, err := WriteMXF(mxfPath, mxfSpec, 17); err != nil {
		t.Fatal(err)
	}

	cm, err := probe.MP4(mp4Path, probe.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	cx, err := probe.MXF(mxfPath)
	if err != nil {
		t.Fatal(err)
	}
	am, ax := cm.Tracks[0], cx.Tracks[0]
	if am.Duration != ax.Duration {
		t.Fatalf("durations differ: mp4=%v mxf=%v", am.Duration, ax.Duration)
	}
	if len(am.Samples) != len(ax.Samples) {
		t.Fatalf("sample counts differ: %d vs %d", len(am.Samples), len(ax.Samples))
	}
	for i := range am.Samples {
		if am.Samples[i].DTS != ax.Samples[i].DTS {
			t.Fatalf("sample %d DTS differs: %v vs %v", i, am.Samples[i].DTS, ax.Samples[i].DTS)
		}
	}
	if am.ColorCandidates[0].Color.Primaries != ax.ColorCandidates[0].Color.Primaries {
		t.Fatalf("colour differs: %+v vs %+v", am.ColorCandidates, ax.ColorCandidates)
	}

	// After normalisation both containers agree on display geometry and colour.
	nm, err := normalize.Track(&am)
	if err != nil {
		t.Fatal(err)
	}
	nx, err := normalize.Track(&ax)
	if err != nil {
		t.Fatal(err)
	}
	if am.DisplayWidth() != ax.DisplayWidth() || am.DisplayHeight() != ax.DisplayHeight() {
		t.Fatalf("display differs: %dx%d vs %dx%d", am.DisplayWidth(), am.DisplayHeight(), ax.DisplayWidth(), ax.DisplayHeight())
	}
	if !am.Crop.Equal(ax.Crop) {
		t.Fatalf("crop differs: %+v vs %+v", am.Crop, ax.Crop)
	}
	if !am.Color.Chosen.Equal(ax.Color.Chosen) {
		t.Fatalf("colour differs: %+v vs %+v", am.Color.Chosen, ax.Color.Chosen)
	}
	if nm.ColorConflicts != nx.ColorConflicts {
		t.Fatalf("colour conflict count differs: %d vs %d", nm.ColorConflicts, nx.ColorConflicts)
	}
}

func readAll(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func TestEditListOffsetsTimeline(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/elst.mp4"
	spec := MP4Spec{
		EditMediaTime: 30000, // one second into the media
		Tracks: []TrackSpec{{
			ID: 1, Kind: "video", Codec: "avc1", Timescale: 30000,
			Frames: 10, FrameDelta: 1000, Width: 64, Height: 64, SampleBytes: 16,
		}},
	}
	if _, err := WriteMP4(path, spec, 3); err != nil {
		t.Fatal(err)
	}
	c, err := probe.MP4(path, probe.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	if !tr.EditOffset.Equal(clock.FromInt(1)) {
		t.Fatalf("edit offset=%v want 1s", tr.EditOffset)
	}
}

func TestEditListClampsSamplesToTimeline(t *testing.T) {
	// With media_time > 0, samples whose media time precedes the edit must be
	// trimmed: the first presented sample is the one the edit starts at.
	dir := t.TempDir()
	path := dir + "/elst.mp4"
	spec := MP4Spec{
		EditMediaTime: 5000,
		Tracks: []TrackSpec{{
			ID: 1, Kind: "video", Codec: "avc1", Timescale: 30000,
			Frames: 10, FrameDelta: 1000, Width: 64, Height: 64, SampleBytes: 16,
		}},
	}
	if _, err := WriteMP4(path, spec, 3); err != nil {
		t.Fatal(err)
	}
	c, err := probe.MP4(path, probe.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Tracks[0].EditOffset; !got.Equal(clock.MustNew(1, 6)) {
		t.Fatalf("edit offset=%v want 1/6 s", got)
	}
}

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
