package ingest

import (
	"testing"

	"github.com/example/mediad/internal/model"
)

func videoTrack() *Track {
	return &Track{
		Kind: "video",
		Video: &VideoTrackSummary{
			TrackID: 1, Width: 1920, Height: 1080,
		},
	}
}

func TestRotationAdjudication(t *testing.T) {
	s := &Summary{Tracks: []*Track{videoTrack()}}
	tr := s.Tracks[0]
	tr.ContainerRot = 90
	tr.Video.Rotation = 270 // track wins
	if err := Adjudicate(s, DefaultPolicy()); err != nil {
		t.Fatal(err)
	}
	if tr.AppliedRotation != 270 {
		t.Fatalf("applied rotation = %d", tr.AppliedRotation)
	}
	if len(tr.ConflictNotes) == 0 {
		t.Fatal("overriding conflict must be recorded")
	}

	// Container-only rotation flows through.
	s2 := &Summary{Tracks: []*Track{videoTrack()}}
	s2.Tracks[0].ContainerRot = 180
	_ = Adjudicate(s2, DefaultPolicy())
	if s2.Tracks[0].AppliedRotation != 180 {
		t.Fatal("container rotation must apply when track has none")
	}
}

func TestInvalidRotationRejected(t *testing.T) {
	s := &Summary{Tracks: []*Track{videoTrack()}}
	s.Tracks[0].Video.Rotation = 45
	err := Adjudicate(s, DefaultPolicy())
	if err == nil {
		t.Fatal("non-orthogonal rotation must be rejected")
	}
	me, ok := model.AsError(err)
	if !ok || me.Kind != model.KindConflict {
		t.Fatalf("wrong error: %v", err)
	}
	if !s.Tracks[0].Rejected {
		t.Fatal("track must be marked rejected")
	}
}

func TestCropAdjudication(t *testing.T) {
	s := &Summary{Tracks: []*Track{videoTrack()}}
	tr := s.Tracks[0]
	tr.ContainerCrop = &Crop{0, 0, 100, 100}
	tr.Video.Crop = &Crop{10, 10, 80, 80}
	_ = Adjudicate(s, DefaultPolicy())
	if tr.AppliedCrop == nil || tr.AppliedCrop.Width != 80 {
		t.Fatalf("applied crop = %+v", tr.AppliedCrop)
	}
}

func TestColorConflict(t *testing.T) {
	s := &Summary{Tracks: []*Track{videoTrack()}}
	tr := s.Tracks[0]
	tr.Video.Colors = []Color{
		{Primaries: 1, Transfer: 1, Matrix: 1, Source: "container"},
		{ICCProfile: true, Source: "track"},
	}
	err := Adjudicate(s, DefaultPolicy())
	if err == nil || !tr.Rejected {
		t.Fatal("mixed NCLC/ICC must reject under default policy")
	}

	// Lenient policy keeps the most local description.
	s2 := &Summary{Tracks: []*Track{videoTrack()}}
	s2.Tracks[0].Video.Colors = tr.Video.Colors
	p := DefaultPolicy()
	p.RejectContradictoryColor = false
	_ = Adjudicate(s2, p)
	if s2.Tracks[0].AppliedColor == nil ||
		!s2.Tracks[0].AppliedColor.ICCProfile {
		t.Fatal("lenient policy must prefer track ICC")
	}

	// Consistent NCLC values are accepted.
	s3 := &Summary{Tracks: []*Track{videoTrack()}}
	s3.Tracks[0].Video.Colors = []Color{
		{Primaries: 1, Source: "container"},
		{Primaries: 1, Source: "track"},
	}
	_ = Adjudicate(s3, DefaultPolicy())
	if s3.Tracks[0].AppliedColor == nil ||
		s3.Tracks[0].AppliedColor.Source != "track" {
		t.Fatal("same-model conflict must prefer track description")
	}
}
