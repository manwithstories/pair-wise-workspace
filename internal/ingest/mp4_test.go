package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example/mediad/internal/model"
)

func TestGenerateAndParseMP4(t *testing.T) {
	dir := t.TempDir()
	spec := FixtureSpec{
		Path:            filepath.Join(dir, "cam1.mp4"),
		Width:           1920,
		Height:          1080,
		VideoTimescale:  30000,
		VideoSamples:    12,
		KeyframeEvery:   3,
		ContainerRotation: 90,
		TrackRotation:   270,
		ContainerCrop:   &Crop{X: 0, Y: 0, Width: 1600, Height: 900},
		TrackCrop:       &Crop{X: 10, Y: 20, Width: 1280, Height: 720},
		ContainerColor:  &Color{Primaries: 1, Transfer: 1, Matrix: 1},
		TrackColor:      &Color{Primaries: 1, Transfer: 1, Matrix: 1},
		AudioSampleRate: 48000,
		AudioChannels:   2,
		AudioLayout:     []string{"L", "R"},
	}
	if _, err := GenerateMP4(spec); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(spec.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	sum, err := ParseMP4(f, spec.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Tracks) != 2 {
		t.Fatalf("tracks = %d", len(sum.Tracks))
	}
	var vid, aud *Track
	for _, tr := range sum.Tracks {
		if tr.Kind == "video" {
			vid = tr
		}
		if tr.Kind == "audio" {
			aud = tr
		}
	}
	if vid == nil || aud == nil {
		t.Fatal("missing video or audio track")
	}
	if vid.Video.Width != 1920 || vid.Video.Height != 1080 {
		t.Fatalf("dimensions = %dx%d", vid.Video.Width, vid.Video.Height)
	}
	if vid.Video.SampleCount != 12 {
		t.Fatalf("sample count = %d", vid.Video.SampleCount)
	}
	if len(vid.Video.KeyframeIndexes) != 4 {
		t.Fatalf("keyframes = %v", vid.Video.KeyframeIndexes)
	}
	if vid.ContainerRot != 90 || vid.TrackRot != 270 {
		t.Fatalf("rotations: container=%d track=%d", vid.ContainerRot, vid.TrackRot)
	}
	if vid.ContainerCrop == nil || vid.Video.Crop == nil ||
		vid.Video.Crop.Width != 1280 {
		t.Fatalf("crops: container=%+v track=%+v", vid.ContainerCrop, vid.Video.Crop)
	}
	if len(vid.Video.Colors) != 2 {
		t.Fatalf("colors = %+v", vid.Video.Colors)
	}
	if aud.Audio.Channels != 2 || aud.Audio.SampleRate != 48000 {
		t.Fatalf("audio = %+v", aud.Audio)
	}

	// Adjudication must keep both raw and applied values.
	if err := Adjudicate(sum, DefaultPolicy()); err != nil {
		t.Fatal(err)
	}
	if vid.AppliedRotation != 270 || vid.AppliedCrop.Width != 1280 {
		t.Fatalf("applied = rot %d crop %+v", vid.AppliedRotation, vid.AppliedCrop)
	}
	if vid.AppliedColor == nil || vid.AppliedColor.Source != "track" {
		t.Fatalf("applied color = %+v", vid.AppliedColor)
	}
}

func TestCorruptFixtures(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		damage FixtureDamage
		kind   model.Kind
	}{
		{"truncated", DamageTruncated, model.KindCorrupt},
		{"bad-sample-table", DamageBadSampleTable, model.KindCorrupt},
		{"missing-keyframes", DamageMissingKeyframes, model.KindCorrupt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".mp4")
			if _, err := GenerateMP4(FixtureSpec{
				Path: path, Width: 640, Height: 360, VideoSamples: 6,
				Damage: tc.damage,
			}); err != nil {
				t.Fatal(err)
			}
			f, _ := os.Open(path)
			_, err := ParseMP4(f, path)
			f.Close()
			if err == nil {
				t.Fatal("corrupt fixture must fail parsing")
			}
			me, ok := model.AsError(err)
			if !ok || me.Kind != tc.kind {
				t.Fatalf("wrong error: %v", err)
			}
		})
	}
}

func TestBadAudioLayoutFixture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "badlayout.mp4")
	if _, err := GenerateMP4(FixtureSpec{
		Path: path, Width: 640, Height: 360, VideoSamples: 6,
		AudioSampleRate: 48000, AudioChannels: 2,
		AudioLayout: []string{"L", "R"},
		Damage:      DamageBadLayout,
	}); err != nil {
		t.Fatal(err)
	}
	sum := mustParse(t, path)
	var aud *AudioTrackLayout
	for _, tr := range sum.Tracks {
		if tr.Kind == "audio" {
			aud = tr.Audio
		}
	}
	if aud == nil {
		t.Fatal("audio track missing")
	}
	if aud.Channels != 4 || len(aud.ChannelLayout) != 2 {
		t.Fatalf("fixture must disagree: ch=%d labels=%v",
			aud.Channels, aud.ChannelLayout)
	}
}

func TestRoundTripSummary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rt.mp4")
	if _, err := GenerateMP4(FixtureSpec{
		Path: path, Width: 1280, Height: 720, VideoSamples: 9,
		KeyframeEvery: 3,
	}); err != nil {
		t.Fatal(err)
	}
	first := mustParse(t, path)
	second := mustParse(t, path)
	if len(first.Tracks) != len(second.Tracks) {
		t.Fatal("re-parse changed track count")
	}
	if len(first.Tracks[0].Video.KeyframeIndexes) !=
		len(second.Tracks[0].Video.KeyframeIndexes) {
		t.Fatal("re-parse changed keyframe index")
	}
}

func mustParse(t *testing.T, path string) *Summary {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := ParseMP4(f, path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
