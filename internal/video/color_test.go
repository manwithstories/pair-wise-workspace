package video

import (
	"testing"

	"mediad/internal/media"
)

func TestValidateRepairsResolutionBackedColorFamilyConflict(t *testing.T) {
	tr := &media.Track{ID: 1, Kind: media.KindVideo, Width: 1280, Height: 720, Color: &media.ColorMetadata{
		Primaries: "bt709", Matrix: "bt2020nc", Transfer: "bt709", BitDepth: 8,
	}}
	rep := Validate(tr)
	if !rep.OK || rep.Repairs != 1 {
		t.Fatalf("rep=%#v", rep)
	}
	if tr.Color.Matrix != "bt709" {
		t.Fatalf("matrix=%s", tr.Color.Matrix)
	}
	if len(tr.Faults) != 1 || tr.Faults[0].Original == "" || tr.Faults[0].RepairBasis == "" {
		t.Fatalf("fault audit incomplete: %#v", tr.Faults)
	}
}

func TestValidateFailsAmbiguousHDRBitDepthConflict(t *testing.T) {
	tr := &media.Track{ID: 1, Kind: media.KindVideo, Width: 1280, Height: 720, Color: &media.ColorMetadata{
		Primaries: "bt709", Matrix: "bt709", Transfer: "pq", BitDepth: 8,
	}}
	rep := Validate(tr)
	if rep.OK || !tr.Failed {
		t.Fatalf("expected failed track, rep=%#v failed=%v", rep.OK, tr.Failed)
	}
}

func TestValidateInfersSDColor(t *testing.T) {
	tr := &media.Track{ID: 1, Kind: media.KindVideo, Width: 720, Height: 480, Color: &media.ColorMetadata{
		Primaries: "bt709", Matrix: "bt601",
	}}
	rep := Validate(tr)
	if !rep.OK {
		t.Fatalf("expected repair: %#v", rep.Faults)
	}
	if tr.Color.Primaries != "bt601" {
		t.Fatalf("primaries=%s", tr.Color.Primaries)
	}
}
