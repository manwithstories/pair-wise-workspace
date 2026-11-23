package metadata

import (
	"testing"

	"mediasync/internal/model"
)

func videoTrack() *model.Track {
	return &model.Track{
		ID: 1, Kind: model.KindVideo, Width: 1920, Height: 1080,
		Meta: model.DisplayMeta{
			SourceWidth: 1920, SourceHeight: 1080,
			Rotation: 90, RotationSrc: model.SourceMP4,
			Colour:    &model.Colour{Primaries: 1, Transfer: 1, Matrix: 1, Known: true},
			ColourSrc: model.SourceMP4,
		},
	}
}

func TestRotationPrecedence(t *testing.T) {
	tr := videoTrack()
	meta, err := Normalize(tr, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if meta.Rotation != 90 || meta.RotationSrc != model.SourceMP4 {
		t.Fatalf("rotation = %d from %s", meta.Rotation, meta.RotationSrc)
	}
	rot := 270
	meta, err = Normalize(tr, Overrides{Rotation: &rot})
	if err != nil {
		t.Fatal(err)
	}
	if meta.Rotation != 270 || meta.RotationSrc != model.SourceExplicit {
		t.Fatalf("explicit rotation = %d from %s", meta.Rotation, meta.RotationSrc)
	}
	// Original claim and explicit claim both recorded.
	if len(meta.Claims) < 1 || meta.Claims[len(meta.Claims)-1].Layer != model.LayerExplicit {
		t.Fatalf("claims = %+v", meta.Claims)
	}
	// The input track is not mutated.
	if tr.Meta.Rotation != 90 {
		t.Fatal("Normalize mutated its input")
	}
}

func TestInvalidRotationRejected(t *testing.T) {
	rot := 45
	if _, err := Normalize(videoTrack(), Overrides{Rotation: &rot}); err == nil {
		t.Fatal("45 degrees should be rejected")
	}
}

func TestTrackRotationBeatsContainerDefaultZero(t *testing.T) {
	tr := videoTrack()
	tr.Meta.Rotation = 0
	tr.Meta.RotationSrc = model.SourceDefault
	meta, err := Normalize(tr, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if meta.Rotation != 0 || meta.RotationSrc != model.SourceDefault {
		t.Fatalf("rotation = %d from %s", meta.Rotation, meta.RotationSrc)
	}
}

func TestCropWithinFrameAccepted(t *testing.T) {
	crop := model.Crop{Left: 8, Right: 8, Top: 4, Bottom: 4}
	meta, err := Normalize(videoTrack(), Overrides{Crop: &crop})
	if err != nil {
		t.Fatal(err)
	}
	if meta.Crop != crop || meta.CropSrc != model.SourceExplicit {
		t.Fatalf("crop = %+v from %s", meta.Crop, meta.CropSrc)
	}
	w, h := DisplayDimensions(meta, 1920, 1080)
	if w != 1080-4-4 || h != 1920-8-8 {
		t.Fatalf("display = %dx%d (rotation must swap axes)", w, h)
	}
}

func TestCropOutOfBoundsRejected(t *testing.T) {
	crop := model.Crop{Left: 1000, Right: 1000}
	_, err := Normalize(videoTrack(), Overrides{Crop: &crop})
	if err == nil || model.CodeOf(err) != model.ErrCropOutOfBounds {
		t.Fatalf("err = %v", err)
	}
	neg := model.Crop{Left: -1}
	if _, err := Normalize(videoTrack(), Overrides{Crop: &neg}); err == nil {
		t.Fatal("negative crop should be rejected")
	}
}

func TestTrackCropOutOfBoundsRejected(t *testing.T) {
	tr := videoTrack()
	tr.Meta.Crop = model.Crop{Top: 600, Bottom: 600}
	if _, err := Normalize(tr, Overrides{}); err == nil {
		t.Fatal("container crop that removes the frame should be rejected")
	}
}

func TestUnknownColourPreservedNotSilentlyOverridden(t *testing.T) {
	tr := videoTrack()
	tr.Meta.Colour = &model.Colour{Primaries: 0xffff, Transfer: 0xfffe, Matrix: 0xfffd, Known: false,
		DefaultInterpretation: "bt709"}
	tr.Meta.Warnings = []string{"unknown colour codes preserved"}
	meta, err := Normalize(tr, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if meta.Colour == nil || meta.Colour.Known {
		t.Fatal("unknown colour must remain marked unknown")
	}
	if meta.Colour.Primaries != 0xffff {
		t.Fatal("unknown colour code was overwritten")
	}
	if meta.Colour.DefaultInterpretation != "bt709" {
		t.Fatal("default interpretation lost")
	}
}

func TestExplicitColourOverrideRecorded(t *testing.T) {
	p := uint16(9)
	meta, err := Normalize(videoTrack(), Overrides{Primaries: &p})
	if err != nil {
		t.Fatal(err)
	}
	if meta.ColourSrc != model.SourceExplicit || meta.Colour.Primaries != 9 {
		t.Fatalf("colour = %+v from %s", meta.Colour, meta.ColourSrc)
	}
	found := false
	for _, c := range meta.Claims {
		if c.Field == "colour" && c.Layer == model.LayerExplicit {
			found = true
		}
	}
	if !found {
		t.Fatal("explicit colour claim not recorded")
	}
}

func TestAbsentColourDefaultsAndWarns(t *testing.T) {
	tr := videoTrack()
	tr.Meta.Colour = nil
	meta, err := Normalize(tr, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if meta.Colour != nil {
		t.Fatal("absent colour must not be invented")
	}
	if len(meta.Warnings) == 0 {
		t.Fatal("expected a warning about absent colour")
	}
}

func TestAudioTrackUntouched(t *testing.T) {
	tr := &model.Track{ID: 2, Kind: model.KindAudio, Width: 0, Height: 0}
	meta, err := Normalize(tr, Overrides{})
	if err != nil {
		t.Fatalf("audio normalisation should not require dimensions: %v", err)
	}
	if meta.Rotation != 0 {
		t.Fatal("audio rotation should be zero")
	}
}

func TestNilTrack(t *testing.T) {
	if _, err := Normalize(nil, Overrides{}); err == nil {
		t.Fatal("nil track should fail")
	}
}

func TestKnownColourSets(t *testing.T) {
	if !knownColour(1, 1, 1) {
		t.Fatal("bt709 should be known")
	}
	if knownColour(0xffff, 1, 1) {
		t.Fatal("reserved primaries should be unknown")
	}
}
