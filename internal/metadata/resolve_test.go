package metadata_test

import (
	"testing"

	"mediarun/internal/mediaerr"
	"mediarun/internal/metadata"
)

func iptr(v int) *int { return &v }

func cropPtr(x, y, w, h int) *metadata.Crop {
	return &metadata.Crop{X: x, Y: y, Width: w, Height: h}
}

func colorPtr(p, tr, m string) *metadata.Color {
	return &metadata.Color{Primaries: p, Transfer: tr, Matrix: m}
}

func TestDefaultWhenNoLayer(t *testing.T) {
	v, err := metadata.Resolve(metadata.Input{TrackID: "video/0", CodedWidth: 1920, CodedHeight: 1080})
	if err != nil {
		t.Fatal(err)
	}
	if v.Rotation.Degrees != 0 || v.Rotation.Source != metadata.SourceDefault {
		t.Fatalf("rotation = %+v, want default 0", v.Rotation)
	}
	if v.Crop != nil {
		t.Fatal("crop must be nil when absent")
	}
	if v.Color.Source != metadata.SourceDefault || v.Color.Primaries != "bt709" {
		t.Fatalf("color = %+v, want default bt709", v.Color)
	}
}

func TestPrecedenceExplicitBeatsTrackBeatsContainer(t *testing.T) {
	in := metadata.Input{
		TrackID: "video/0", CodedWidth: 1920, CodedHeight: 1080,
		ContainerLayer: &metadata.LayerValues{Rotation: iptr(90)},
		TrackLayer:     &metadata.LayerValues{Rotation: iptr(180)},
		Override:       &metadata.LayerValues{Rotation: iptr(270)},
	}
	v, err := metadata.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	if v.Rotation.Degrees != 270 || v.Rotation.Source != metadata.SourceOverride {
		t.Fatalf("rotation = %+v, want 270 from override", v.Rotation)
	}
	// Without override, track must beat container.
	in.Override = nil
	v, err = metadata.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	if v.Rotation.Degrees != 180 || v.Rotation.Source != metadata.SourceTrack {
		t.Fatalf("rotation = %+v, want 180 from track", v.Rotation)
	}
}

func TestConflictRecordedWithLosers(t *testing.T) {
	in := metadata.Input{
		TrackID: "video/0", CodedWidth: 1920, CodedHeight: 1080,
		ContainerLayer: &metadata.LayerValues{Rotation: iptr(90)},
		TrackLayer:     &metadata.LayerValues{Rotation: iptr(270)},
	}
	v, err := metadata.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	var rotDec *metadata.Decision
	for i := range v.Decisions {
		if v.Decisions[i].Field == "rotation" {
			rotDec = &v.Decisions[i]
		}
	}
	if rotDec == nil || !rotDec.Conflict || len(rotDec.Losers) == 0 {
		t.Fatalf("expected a conflict decision, got %+v", v.Decisions)
	}
	if rotDec.Winner != metadata.SourceTrack {
		t.Fatalf("winner = %s, want track", rotDec.Winner)
	}
}

func TestSameValueNoConflict(t *testing.T) {
	in := metadata.Input{
		TrackID: "video/0", CodedWidth: 1920, CodedHeight: 1080,
		ContainerLayer: &metadata.LayerValues{Rotation: iptr(90)},
		TrackLayer:     &metadata.LayerValues{Rotation: iptr(90)},
	}
	v, err := metadata.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range v.Decisions {
		if d.Field == "rotation" && d.Conflict {
			t.Fatal("equal values across layers must not be a conflict")
		}
	}
}

func TestCropInBoundsAccepted(t *testing.T) {
	in := metadata.Input{
		TrackID: "video/0", CodedWidth: 1920, CodedHeight: 1080,
		TrackLayer: &metadata.LayerValues{Crop: cropPtr(10, 20, 100, 200)},
	}
	v, err := metadata.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	if v.Crop == nil || v.Crop.Source != metadata.SourceTrack || v.Crop.Width != 100 {
		t.Fatalf("crop = %+v", v.Crop)
	}
}

func TestCropOutOfBoundsRejected(t *testing.T) {
	cases := []metadata.Crop{
		{X: 0, Y: 0, Width: 2000, Height: 1080}, // wider than frame
		{X: 1900, Y: 0, Width: 100, Height: 10}, // extends past right edge
		{X: 0, Y: 0, Width: 100, Height: 0},     // non-positive extent
		{X: -5, Y: 0, Width: 100, Height: 100},  // negative origin
	}
	for _, c := range cases {
		in := metadata.Input{
			TrackID: "video/0", CodedWidth: 1920, CodedHeight: 1080,
			Override: &metadata.LayerValues{Crop: &c},
		}
		v, err := metadata.Resolve(in)
		if err == nil {
			t.Fatalf("crop %+v must be rejected, got view %+v", c, v)
		}
		if !mediaerr.Is(err, mediaerr.CodeCropOutOfBounds) {
			t.Fatalf("want CROP_OUT_OF_BOUNDS, got %v", err)
		}
	}
}

func TestColorArbitrationAndDefault(t *testing.T) {
	in := metadata.Input{
		TrackID: "video/0", CodedWidth: 640, CodedHeight: 360,
		ContainerLayer: &metadata.LayerValues{Color: colorPtr("bt601", "bt601", "smpte170m")},
		Override:       &metadata.LayerValues{Color: colorPtr("bt2020", "bt2020", "bt2020nc")},
	}
	v, err := metadata.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	if v.Color.Primaries != "bt2020" || v.Color.Source != metadata.SourceOverride {
		t.Fatalf("color = %+v", v.Color)
	}
}

func TestInvalidRotationRejected(t *testing.T) {
	in := metadata.Input{
		TrackID: "video/0", CodedWidth: 100, CodedHeight: 100,
		Override: &metadata.LayerValues{Rotation: iptr(45)},
	}
	_, err := metadata.Resolve(in)
	if !mediaerr.Is(err, mediaerr.CodeInvalidConfig) {
		t.Fatalf("want INVALID_CONFIG for 45deg rotation, got %v", err)
	}
}

func TestNegative90NormalizedTo270(t *testing.T) {
	in := metadata.Input{
		TrackID: "video/0", CodedWidth: 100, CodedHeight: 100,
		TrackLayer: &metadata.LayerValues{Rotation: iptr(-90)},
	}
	v, err := metadata.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	if v.Rotation.Degrees != 270 {
		t.Fatalf("rotation = %d, want 270", v.Rotation.Degrees)
	}
}
