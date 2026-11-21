package model

import (
	"testing"

	"mediad/internal/clock"
)

func TestTrackValidate(t *testing.T) {
	tr := Track{
		ID: 1, Kind: Video, Codec: "avc1", CodedWidth: 1920, CodedHeight: 1080,
		Rotation: 90, Crop: Crop{Left: 4, Right: 4}, Duration: clock.FromInt(10),
	}
	if err := tr.Validate(); err != nil {
		t.Fatalf("valid track rejected: %v", err)
	}
	if tr.DisplayWidth() != 1080 || tr.DisplayHeight() != 1912 {
		t.Fatalf("display %dx%d want 1080x1912", tr.DisplayWidth(), tr.DisplayHeight())
	}
}

func TestTrackCropOutOfRange(t *testing.T) {
	tr := Track{
		ID: 2, Kind: Video, CodedWidth: 100, CodedHeight: 100,
		Crop: Crop{Left: 60, Right: 40},
	}
	err := tr.Validate()
	if err == nil {
		t.Fatal("expected out-of-range crop error")
	}
	if KindOf(err) != KindInvalidMedia {
		t.Fatalf("kind=%v", KindOf(err))
	}
	e, _ := AsError(err)
	if e.TrackID != 2 {
		t.Fatalf("track=%d", e.TrackID)
	}
}

func TestTrackValidateNonMonotonic(t *testing.T) {
	mk := func(ts int64) Sample {
		return Sample{Index: int(ts), DTS: clock.FromInt(ts), Duration: clock.MustNew(1, 30)}
	}
	tr := Track{ID: 3, Kind: Video, CodedWidth: 10, CodedHeight: 10,
		Samples: []Sample{mk(0), mk(2), mk(1)}}
	if err := tr.Validate(); KindOf(err) != KindInvalidMedia {
		t.Fatalf("expected invalid media, got %v", err)
	}
}

func TestColorDefault(t *testing.T) {
	d := DefaultColor()
	if d.Primaries != 2 || d.Matrix != 2 || d.Description != "default" {
		t.Fatalf("default colour = %+v", d)
	}
	if !d.Equal(ColorSpace{Primaries: 2, Transfer: 2, Matrix: 2, FullRange: false}) {
		t.Fatal("Equal should ignore description")
	}
}
