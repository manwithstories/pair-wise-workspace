package meta

import (
	"testing"

	"relaycheck/internal/media"
)

func rot(d int) *Rotation { return &Rotation{Degrees: d} }

func TestPrecedenceOverrideTrackContainerDefault(t *testing.T) {
	track := Layer{Rotation: rot(180)}
	container := Layer{Rotation: rot(270)}
	override := Layer{Rotation: rot(90)}
	rt, f := ReconcileTrack(ReconcileInput{
		TrackID: 7, TimeScale: 1000, Duration: 4000,
		Override: override, Track: track, Container: container, Default: DefaultLayer(),
	})
	if f != nil {
		t.Fatal(f)
	}
	if rt.Rotation.Degrees != 90 {
		t.Fatalf("override must win, got %d", rt.Rotation.Degrees)
	}
	if len(rt.Conflicts) != 1 || rt.Conflicts[0].Chosen != "override" {
		t.Fatalf("expected one rotation conflict choosing override, got %+v", rt.Conflicts)
	}
}

func TestTrackBeatsContainerAndRecordsConflict(t *testing.T) {
	tr, f := ReconcileTrack(ReconcileInput{
		TrackID: 1, TimeScale: 48000, Duration: 192000,
		Track:     Layer{Color: &Color{Primaries: "bt2020", Known: true}},
		Container: Layer{Color: &Color{Primaries: "bt709", Known: true}},
		Default:   DefaultLayer(),
	})
	if f != nil {
		t.Fatal(f)
	}
	if tr.Color.Primaries != "bt2020" {
		t.Fatalf("track color must win, got %s", tr.Color.Primaries)
	}
	if len(tr.Conflicts) != 1 || tr.Conflicts[0].Attribute != "color" {
		t.Fatalf("want color conflict, got %+v", tr.Conflicts)
	}
}

func TestDefaultAppliesWhenNoOtherClaim(t *testing.T) {
	tr, f := ReconcileTrack(ReconcileInput{TrackID: 1, TimeScale: 48000, Duration: 100})
	if f != nil {
		t.Fatal(f)
	}
	if tr.Rotation.Degrees != 0 || tr.Color == nil || !tr.Color.Known {
		t.Fatalf("explicit defaults not applied: %+v", tr)
	}
}

func TestOverrideCanRescueIllegalTrackValue(t *testing.T) {
	_, f := ReconcileTrack(ReconcileInput{
		TrackID: 2, TimeScale: 48000, Duration: 100,
		Track:    Layer{Rotation: rot(45)},
		Override: Layer{Rotation: rot(90)},
	})
	if f != nil {
		t.Fatalf("valid override should rescue illegal track claim: %v", f)
	}
}

func TestValidationFaultsCarryTrackAndRange(t *testing.T) {
	cases := []struct {
		name string
		in   ReconcileInput
		code media.Code
	}{
		{"illegal rotation", ReconcileInput{TrackID: 3, TimeScale: 48000, Duration: 480, Track: Layer{Rotation: rot(45)}}, media.CodeInvalidRotation},
		{"bad crop", ReconcileInput{TrackID: 3, TimeScale: 48000, Duration: 480, Track: Layer{Crop: &Crop{X: 10, W: 2000, FrameW: 1920}}}, media.CodeInvalidCrop},
		{"unknown color", ReconcileInput{TrackID: 3, TimeScale: 48000, Duration: 480, Track: Layer{Color: &Color{Known: false, RawMarker: 9}}}, media.CodeUnknownColor},
		{"zero timescale", ReconcileInput{TrackID: 3, Duration: 480}, media.CodeInvalidTimebase},
	}
	for _, c := range cases {
		_, f := ReconcileTrack(c.in)
		if f == nil {
			t.Fatalf("%s: expected fault %s", c.name, c.code)
		}
		if f.Code != c.code {
			t.Fatalf("%s: code=%s want %s", c.name, f.Code, c.code)
		}
		if f.TrackID != 3 {
			t.Fatalf("%s: fault missing track id: %+v", c.name, f)
		}
		if c.code != media.CodeInvalidTimebase && f.Range == nil {
			t.Fatalf("%s: fault missing time range", c.name)
		}
	}
}

func TestCropBounds(t *testing.T) {
	good := Crop{X: 10, Y: 10, W: 1900, H: 1060, FrameW: 1920, FrameH: 1080}
	if !good.InBounds() {
		t.Fatal("expected crop in bounds")
	}
	if (Crop{W: 0, H: 10}.InBounds()) {
		t.Fatal("zero width must be out of bounds")
	}
	if (Crop{X: -1, W: 10, H: 10}.InBounds()) {
		t.Fatal("negative x must be out of bounds")
	}
}
