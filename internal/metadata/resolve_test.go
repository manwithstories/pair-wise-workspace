package metadata

import (
	"strings"
	"testing"

	"avpipe/internal/media"
)

func TestRotationOnlyRightAnglesAccepted(t *testing.T) {
	for _, deg := range []int{0, 90, 180, 270, -90, 450} {
		di, err := Resolve(VideoFacts{
			TrackID: "v1", Width: 1920, Height: 1080,
			HasTrackRot: true, TrackRotation: deg,
		}, Explicit{})
		if err != nil {
			t.Fatalf("%d: %v", deg, err)
		}
		if di.Rotation%90 != 0 {
			t.Fatalf("%d produced non-right angle %d", deg, di.Rotation)
		}
		if di.Rotation < 0 || di.Rotation >= 360 {
			t.Fatalf("%d produced out-of-range %d", deg, di.Rotation)
		}
	}
	_, err := Resolve(VideoFacts{
		TrackID: "v1", Width: 1920, Height: 1080,
		HasTrackRot: true, TrackRotation: 45,
	}, Explicit{})
	if media.KindOf(err) != media.KindCorrupt {
		t.Fatalf("45deg kind = %v", media.KindOf(err))
	}
	if !strings.Contains(err.Error(), "multiple of 90") {
		t.Fatalf("message: %v", err)
	}
}

func TestExplicitRotationWinsOverTrack(t *testing.T) {
	di, err := Resolve(VideoFacts{
		TrackID: "v1", Width: 1920, Height: 1080,
		HasTrackRot: true, TrackRotation: 90,
	}, Explicit{Rotation: intp(180)})
	if err != nil {
		t.Fatal(err)
	}
	if di.Rotation != 180 {
		t.Fatalf("rotation = %d", di.Rotation)
	}
	if di.Provenance["rotation"] != "explicit" {
		t.Fatalf("provenance = %q", di.Provenance["rotation"])
	}
}

func TestCropOutOfBoundsInvalidates(t *testing.T) {
	// A crop whose width exceeds the coded width is not a legal display crop.
	_, err := Resolve(VideoFacts{
		TrackID: "v1", Width: 160, Height: 90,
		TrackCrop: &media.CropRect{W: 170, H: 90, X: 0, Y: 0},
	}, Explicit{})
	if media.KindOf(err) != media.KindCorrupt {
		t.Fatalf("kind = %v (%v)", media.KindOf(err), err)
	}
	if !strings.Contains(err.Error(), "exceeds coded frame") {
		t.Fatalf("message: %v", err)
	}
}

func TestCropWithinBoundsAccepted(t *testing.T) {
	di, err := Resolve(VideoFacts{
		TrackID: "v1", Width: 1920, Height: 1080,
		ContainerCrop: &media.CropRect{W: 1888, H: 1062, X: 0, Y: 0},
	}, Explicit{})
	if err != nil {
		t.Fatal(err)
	}
	if di.Crop == nil || di.Crop.W != 1888 || di.Crop.H != 1062 {
		t.Fatalf("crop = %+v", di.Crop)
	}
	if di.Provenance["crop"] != "container" {
		t.Fatalf("provenance = %q", di.Provenance["crop"])
	}
}

func TestColorPrecedenceAndConflicts(t *testing.T) {
	di, err := Resolve(VideoFacts{
		TrackID: "v1", Width: 1920, Height: 1080,
		Color: []ColorSource{
			{Source: SourceBitstream, Color: media.ColorInfo{Primaries: "bt709", Transfer: "smpte2084", Space: "bt709"}},
			{Source: SourceContainer, Color: media.ColorInfo{Primaries: "bt2020", Transfer: "arib-std-b67"}},
			{Source: SourceTrack, Color: media.ColorInfo{Primaries: "bt709"}},
		},
	}, Explicit{})
	if err != nil {
		t.Fatal(err)
	}
	// Track wins primaries, container supplies transfer, bitstream supplies space.
	if di.Color.Primaries != "bt709" {
		t.Fatalf("primaries = %q", di.Color.Primaries)
	}
	if di.Color.Transfer != "arib-std-b67" {
		t.Fatalf("transfer = %q", di.Color.Transfer)
	}
	if di.Color.Space != "bt709" {
		t.Fatalf("space = %q", di.Color.Space)
	}
	if di.Provenance["color.primaries"] != "track" {
		t.Fatalf("primaries provenance = %q", di.Provenance["color.primaries"])
	}
	if di.Provenance["color.transfer"] != "container" {
		t.Fatalf("transfer provenance = %q", di.Provenance["color.transfer"])
	}
	// Container bt2020 and bitstream smpte2084 lose to the track bt709.
	var sawPrim, sawTrc bool
	for _, c := range di.Conflicts {
		if strings.Contains(c, "container=bt2020") {
			sawPrim = true
		}
		if strings.Contains(c, "bitstream=smpte2084") {
			sawTrc = true
		}
	}
	if !sawPrim || !sawTrc {
		t.Fatalf("conflicts missing losers: %v", di.Conflicts)
	}
}

func TestColorExplicitWins(t *testing.T) {
	di, err := Resolve(VideoFacts{
		TrackID: "v1", Width: 1920, Height: 1080,
		Color: []ColorSource{
			{Source: SourceTrack, Color: media.ColorInfo{Range: "tv"}},
		},
	}, Explicit{Color: &media.ColorInfo{Range: "pc"}})
	if err != nil {
		t.Fatal(err)
	}
	if di.Color.Range != "pc" {
		t.Fatalf("range = %q", di.Color.Range)
	}
	if len(di.Conflicts) != 1 || !strings.Contains(di.Conflicts[0], "track=tv") {
		t.Fatalf("conflicts = %v", di.Conflicts)
	}
}

func TestNoColorMeansEmpty(t *testing.T) {
	di, err := Resolve(VideoFacts{TrackID: "v1", Width: 320, Height: 180}, Explicit{})
	if err != nil {
		t.Fatal(err)
	}
	if di.Color != (media.ColorInfo{}) {
		t.Fatalf("color = %+v", di.Color)
	}
}

func TestNegativeCropRejected(t *testing.T) {
	_, err := Resolve(VideoFacts{
		TrackID: "v1", Width: 320, Height: 180,
		TrackCrop: &media.CropRect{W: 100, H: 100, X: -5, Y: 0},
	}, Explicit{})
	if media.KindOf(err) != media.KindCorrupt {
		t.Fatalf("kind = %v", media.KindOf(err))
	}
}

func intp(v int) *int { return &v }
