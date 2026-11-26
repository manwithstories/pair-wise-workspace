package metadata_test

import (
	"testing"

	"medialab/internal/format"
	"medialab/internal/metadata"
)

func TestColorPriorityKeepsDiscardedCandidates(t *testing.T) {
	containerColor := &format.ColorDescription{Primaries: "bt2020", Source: metadata.SourceContainer}
	trackColor := &format.ColorDescription{Primaries: "bt709", Source: metadata.SourceTrack}
	explicit := &format.ColorDescription{Primaries: "display-p3", Source: metadata.SourceExplicit}
	c := &format.Container{
		ContainerColor: containerColor,
		Tracks:         []*format.Track{{ID: 1, Color: trackColor, Width: 16, Height: 16}},
	}
	rep := metadata.Resolve(c, metadata.Override{TrackID: 1, Color: explicit})
	d := rep.Tracks[0].Color
	if d.Priority != metadata.SourceExplicit || d.Selected.Primaries != "display-p3" {
		t.Fatalf("decision=%+v", d)
	}
	if len(d.Discarded) != 2 {
		t.Fatalf("discarded=%+v, want track and container", d.Discarded)
	}
}

func TestIllegalRotationAndCropInvalidateOnlyTheirTrack(t *testing.T) {
	c := &format.Container{Tracks: []*format.Track{
		{ID: 1, Kind: format.KindVideo, Width: 320, Height: 180, Rotation: 45, RotationAt: 77},
		{ID: 2, Kind: format.KindVideo, Width: 64, Height: 64, Crop: &format.Crop{Left: 40, Right: 40, Offset: 88}},
		{ID: 3, Kind: format.KindVideo, Width: 320, Height: 180, Rotation: 90},
	}}
	rep := metadata.Resolve(c)
	if rep.Tracks[0].Valid || rep.Tracks[0].Fault.Offset != 77 {
		t.Fatalf("rotation track=%+v", rep.Tracks[0])
	}
	if rep.Tracks[1].Valid || rep.Tracks[1].Fault.Offset != 88 {
		t.Fatalf("crop track=%+v", rep.Tracks[1])
	}
	if !rep.Tracks[2].Valid {
		t.Fatalf("healthy track polluted: %+v", rep.Tracks[2].Fault)
	}
	if len(rep.ValidVideoTracks()) != 1 || rep.ValidVideoTracks()[0].Track.ID != 3 {
		t.Fatalf("valid tracks=%+v", rep.ValidVideoTracks())
	}
}
