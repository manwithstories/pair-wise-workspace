package normalize

import (
	"testing"

	"mediad/internal/clock"
	"mediad/internal/model"
)

func TestRotationNormalisation(t *testing.T) {
	cases := map[int]int{-270: 90, -180: 0, -90: 270, 0: 0, 90: 90, 180: 180, 270: 270, 360: 0}
	for in, want := range cases {
		tr := model.Track{ID: 1, Kind: model.Video, CodedWidth: 1920, CodedHeight: 1080, Rotation: in}
		if _, err := Track(&tr); err != nil {
			t.Fatalf("rotation %d rejected: %v", in, err)
		}
		if tr.Rotation != want {
			t.Errorf("rotation %d -> %d want %d", in, tr.Rotation, want)
		}
	}
}

func TestRotationRejectsNonRightAngle(t *testing.T) {
	tr := model.Track{ID: 7, Kind: model.Video, CodedWidth: 640, CodedHeight: 480, Rotation: 45}
	_, err := Track(&tr)
	if err == nil {
		t.Fatal("expected rejection of 45 degree rotation")
	}
	if model.KindOf(err) != model.KindInvalidMedia {
		t.Fatalf("kind=%v", model.KindOf(err))
	}
	e, _ := model.AsError(err)
	if e.TrackID != 7 {
		t.Fatalf("track=%d", e.TrackID)
	}
}

func TestCropOutOfRangeReportsTrackAndRange(t *testing.T) {
	tr := model.Track{
		ID: 9, Kind: model.Video, CodedWidth: 1280, CodedHeight: 720,
		Start: clock.FromInt(3), Duration: clock.FromInt(2),
		Crop: model.Crop{Left: 700, Right: 600},
	}
	_, err := Track(&tr)
	if err == nil {
		t.Fatal("expected crop error")
	}
	e, _ := model.AsError(err)
	if e.TrackID != 9 {
		t.Fatalf("track=%d", e.TrackID)
	}
	if got := e.Detail; !contains(got, "720") || !contains(got, "[3, 5]") {
		t.Fatalf("detail %q missing dimensions or time range", got)
	}
}

func TestColorPrecedence(t *testing.T) {
	defaultColor := model.DefaultColor()
	trackColor := model.ColorSpace{Primaries: 9, Transfer: 16, Matrix: 9, Description: "nclx"}
	explicit := model.ColorSpace{Primaries: 1, Transfer: 1, Matrix: 1, Description: "explicit"}

	dec := ResolveColor([]model.ColorCandidate{
		{Layer: model.ColorLayerContainer, Color: defaultColor, Origin: "absent"},
		{Layer: model.ColorLayerTrack, Color: trackColor, Origin: "colr/nclx"},
	})
	if !dec.Chosen.Equal(trackColor) {
		t.Fatalf("track should beat container: %+v", dec.Chosen)
	}
	if dec.Conflicts != 1 {
		t.Fatalf("conflicts=%d want 1", dec.Conflicts)
	}

	dec = ResolveColor([]model.ColorCandidate{
		{Layer: model.ColorLayerTrack, Color: trackColor, Origin: "colr/nclx"},
		{Layer: model.ColorLayerExplicit, Color: explicit, Origin: "flag"},
		{Layer: model.ColorLayerContainer, Color: defaultColor, Origin: "absent"},
	})
	if !dec.Chosen.Equal(explicit) {
		t.Fatalf("explicit should win: %+v", dec.Chosen)
	}
	if dec.Conflicts != 2 {
		t.Fatalf("conflicts=%d want 2", dec.Conflicts)
	}
	if len(dec.Trace) != 3 {
		t.Fatalf("trace length %d", len(dec.Trace))
	}
}

func TestColorAbsentRecordsDefault(t *testing.T) {
	dec := ResolveColor(nil)
	if dec.Chosen.Primaries != 2 || dec.Chosen.Description != "default" {
		t.Fatalf("absent colour = %+v", dec.Chosen)
	}
	if len(dec.Trace) != 1 || dec.Trace[0].Origin != "absent" {
		t.Fatalf("trace = %+v", dec.Trace)
	}
}

func TestCropOnAudioRejected(t *testing.T) {
	tr := model.Track{ID: 2, Kind: model.Audio, Crop: model.Crop{Left: 1}}
	if _, err := Track(&tr); err == nil {
		t.Fatal("crop on audio should be rejected")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
