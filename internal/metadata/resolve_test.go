package metadata

import (
	"testing"

	"mediacore/internal/media"
)

func TestResolvePriorityAndConflicts(t *testing.T) {
	ovThree := 3
	c := &media.Container{
		Format: "mp4", HasRotate: true, Rotation: 1,
		Color: media.Color{Present: true, Primaries: 9, Transfer: 16, Matrix: 9, Source: "container"},
		Tracks: []*media.Track{{
			ID: media.TrackID{Container: "mp4", Value: 1}, Width: 1920, Height: 1080,
			HasRotate: true, Rotation: 3,
			Color: media.Color{Present: true, Primaries: 1, Transfer: 1, Matrix: 1, Source: "track"},
			Crop:  &media.Crop{X: 100, Y: 50, Width: 800, Height: 600},
		}},
	}
	r, err := Resolve(c, Override{Rotation: &ovThree})
	if err != nil {
		t.Fatal(err)
	}
	e := r.Entries[0]
	if e.Rotation != 3 || e.RotationFrom != "override" {
		t.Fatalf("rotation=%d from=%s", e.Rotation, e.RotationFrom)
	}
	if len(e.RotationConflicts) != 2 {
		t.Fatalf("conflicts=%v", e.RotationConflicts)
	}
	if e.Crop.Width != 800 || e.CropFrom != "track" {
		t.Fatalf("crop=%v from=%s", e.Crop, e.CropFrom)
	}
	if e.ColorFrom != "track" || len(e.ColorConflicts) != 1 {
		t.Fatalf("color from=%s conflicts=%v", e.ColorFrom, e.ColorConflicts)
	}
}

func TestResolveRejectsOutOfFrameCrop(t *testing.T) {
	c := &media.Container{Format: "mp4", Tracks: []*media.Track{{
		ID: media.TrackID{Container: "mp4", Value: 2}, Width: 1920, Height: 1080,
		Crop: &media.Crop{X: 1500, Y: 0, Width: 800, Height: 600},
	}}}
	_, err := Resolve(c, Override{})
	if media.ClassOf(err) != media.ClassMetadataConflict {
		t.Fatalf("err=%v class=%s", err, media.ClassOf(err))
	}
}

func TestResolveUnknownColorDefaults(t *testing.T) {
	c := &media.Container{Format: "mp4", Tracks: []*media.Track{{
		ID: media.TrackID{Container: "mp4", Value: 1}, Width: 10, Height: 10,
		Color: media.Color{Present: true, Source: "track"},
	}}}
	r, err := Resolve(c, Override{})
	if err != nil {
		t.Fatal(err)
	}
	e := r.Entries[0]
	if e.Color.Primaries != media.DefaultColor.Primaries || e.ColorFrom != "track" || len(e.Warnings) == 0 {
		t.Fatalf("color=%v from=%s warnings=%v", e.Color, e.ColorFrom, e.Warnings)
	}
}

func TestStrictRotationRejectsNonQuarterOverride(t *testing.T) {
	c := &media.Container{Format: "mp4", Tracks: []*media.Track{{
		ID: media.TrackID{Container: "mp4", Value: 1}, Width: 10, Height: 10,
	}}}
	bad := 5
	if _, err := Resolve(c, Override{Rotation: &bad, StrictRotation: true}); media.ClassOf(err) != media.ClassMetadataConflict {
		t.Fatalf("class=%s err=%v", media.ClassOf(err), err)
	}
	if _, err := Resolve(c, Override{Rotation: &bad}); err != nil {
		t.Fatalf("non-strict override should normalize: %v", err)
	}
}

func TestNormalizeRotation(t *testing.T) {
	cases := map[int]int{-1: 3, 0: 0, 1: 1, 3: 3, 4: 0, 5: 1, 450: 2}
	for in, want := range cases {
		if got := normalizeRotation(in); got != want {
			t.Fatalf("normalize(%d)=%d want %d", in, got, want)
		}
	}
}
