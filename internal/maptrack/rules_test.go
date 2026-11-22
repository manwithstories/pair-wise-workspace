package maptrack

import (
	"testing"

	"mediabridge/internal/fault"
	"mediabridge/internal/media"
)

func assetWithTracks() *media.Asset {
	return &media.Asset{
		ID: "A1",
		Tracks: []media.Track{
			{Index: 0, ID: 1, Kind: media.KindVideo},
			{Index: 1, ID: 2, Kind: media.KindAudio},
			{Index: 2, ID: 3, Kind: media.KindAux},
		},
	}
}

func TestApplySelectsPrimaryAndAux(t *testing.T) {
	rules := Rule{
		Auxiliary: []AuxSlot{{Name: "timecode", TrackID: 3}},
	}
	plan, err := Apply(rules, assetWithTracks(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Assignments) != 3 {
		t.Fatalf("assignments = %d", len(plan.Assignments))
	}
	want := map[string]int{"video": 0, "audio": 1, "timecode": 2}
	for _, a := range plan.Assignments {
		if got, ok := want[a.Slot]; !ok || got != a.TrackIndex {
			t.Fatalf("slot %s → %d", a.Slot, a.TrackIndex)
		}
	}
}

func TestApplyPinsByID(t *testing.T) {
	asset := assetWithTracks()
	// Add a second video track and pin the first by id.
	asset.Tracks = append(asset.Tracks, media.Track{Index: 3, ID: 9, Kind: media.KindVideo})
	plan, err := Apply(Rule{VideoTrackID: 9, DropUnmapped: true}, asset, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plan.Assignments {
		if a.Slot == "video" && a.TrackIndex != 3 {
			t.Fatalf("video mapped to %d want 3", a.TrackIndex)
		}
	}
	if len(plan.Dropped) == 0 {
		t.Fatal("expected dropped tracks")
	}
}

func TestApplyMissingVideoRejects(t *testing.T) {
	asset := &media.Asset{ID: "A1", Tracks: []media.Track{{Index: 0, ID: 2, Kind: media.KindAudio}}}
	_, err := Apply(Rule{}, asset, nil)
	if fault.CodeOf(err) != fault.CodeMissingMapping {
		t.Fatalf("code = %v", fault.CodeOf(err))
	}
	e, _ := fault.As(err)
	if e.Loc.AssetID != "A1" {
		t.Fatalf("missing asset id: %+v", e.Loc)
	}
}

func TestApplyDuplicateRejects(t *testing.T) {
	rules := Rule{Auxiliary: []AuxSlot{
		{Name: "a", TrackID: 3},
		{Name: "b", TrackID: 3},
	}}
	_, err := Apply(rules, assetWithTracks(), nil)
	if fault.CodeOf(err) != fault.CodeDuplicateMapping {
		t.Fatalf("code = %v (%v)", fault.CodeOf(err), err)
	}
}

func TestApplyUnmappedRejectsWhenNotDropping(t *testing.T) {
	_, err := Apply(Rule{}, assetWithTracks(), nil)
	if fault.CodeOf(err) != fault.CodeMissingMapping {
		t.Fatalf("code = %v", fault.CodeOf(err))
	}
	plan, err := Apply(Rule{DropUnmapped: true}, assetWithTracks(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Dropped) != 1 || plan.Dropped[0] != 2 {
		t.Fatalf("dropped = %v", plan.Dropped)
	}
}

func TestApplyRTPAuxMapping(t *testing.T) {
	rules := Rule{
		Auxiliary: []AuxSlot{{Name: "klv", Source: "aux/klv.rtpd"}},
	}
	aux := []AuxTrack{{Source: "aux/klv.rtpd", Index: 2, ID: 77, Kind: media.KindAux}}
	plan, err := Apply(rules, assetWithTracks(), aux)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range plan.Assignments {
		if a.Slot == "klv" {
			found = true
			if a.TrackIndex != 2 || a.TrackID != 77 {
				t.Fatalf("klv = %+v", a)
			}
		}
	}
	if !found {
		t.Fatal("klv slot missing")
	}
}

func TestApplyRTPAuxMissingSource(t *testing.T) {
	rules := Rule{Auxiliary: []AuxSlot{{Name: "klv", Source: "missing.rtpd"}}}
	_, err := Apply(rules, assetWithTracks(), nil)
	if fault.CodeOf(err) != fault.CodeMissingMapping {
		t.Fatalf("code = %v", fault.CodeOf(err))
	}
}
