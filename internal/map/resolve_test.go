package trackmap

import (
	"testing"

	"mediasync/internal/model"
)

func tracks() []*model.Track {
	return []*model.Track{
		{ID: 1, Kind: model.KindVideo, Codec: "avc1"},
		{ID: 2, Kind: model.KindAudio, Codec: "mp4a"},
		{ID: 3, Kind: model.KindAudio, Codec: "mp4a"},
		{ID: 4, Kind: model.KindData, Codec: "meta"},
	}
}

func TestExplicitMappingWins(t *testing.T) {
	id := uint32(3)
	res, err := Resolve([]Target{
		{Name: "program-video", Kind: model.KindVideo, Required: true},
		{Name: "program-audio", Kind: model.KindAudio, Required: true, Explicit: &id},
	}, tracks(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Selections {
		if s.Target.Name == "program-audio" {
			if s.Track.ID != 3 || s.Rule != "explicit" {
				t.Fatalf("audio mapped to %d via %s", s.Track.ID, s.Rule)
			}
		}
	}
}

func TestTrackIDHintBeforeType(t *testing.T) {
	res, err := Resolve([]Target{
		{Name: "program-audio", Kind: model.KindAudio, Required: true},
	}, tracks(), Options{IDHints: map[string]uint32{"program-audio": 3}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Selections[0].Track.ID != 3 || res.Selections[0].Rule != "track-id" {
		t.Fatalf("mapped %d via %s", res.Selections[0].Track.ID, res.Selections[0].Rule)
	}
}

func TestTypeFallbackPicksLowestIDAndNotes(t *testing.T) {
	res, err := Resolve([]Target{{Name: "program-audio", Kind: model.KindAudio, Required: true}}, tracks(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Selections[0].Track.ID != 2 || res.Selections[0].Rule != "type" {
		t.Fatalf("mapped %d via %s", res.Selections[0].Track.ID, res.Selections[0].Rule)
	}
	if len(res.Notes) == 0 {
		t.Fatal("ambiguity should be noted")
	}
}

func TestDuplicateTargetRejected(t *testing.T) {
	_, err := Resolve([]Target{
		{Name: "program-video", Kind: model.KindVideo, Required: true},
		{Name: "program-video", Kind: model.KindVideo, Required: true},
	}, tracks(), Options{})
	if err == nil || model.CodeOf(err) != model.ErrTrackMapDuplicate {
		t.Fatalf("err = %v code %s", err, model.CodeOf(err))
	}
}

func TestDuplicateSourceRejected(t *testing.T) {
	id2 := uint32(2)
	id2b := uint32(2)
	_, err := Resolve([]Target{
		{Name: "left", Kind: model.KindAudio, Required: true, Explicit: &id2},
		{Name: "right", Kind: model.KindAudio, Required: true, Explicit: &id2b},
	}, tracks(), Options{})
	if err == nil || model.CodeOf(err) != model.ErrTrackMapDuplicate {
		t.Fatalf("err = %v", err)
	}
	if model.ExitCode(err) != model.ExitDataError {
		t.Fatalf("exit = %d", model.ExitCode(err))
	}
}

func TestMissingRequiredTrackRejected(t *testing.T) {
	_, err := Resolve([]Target{{Name: "program-video", Kind: model.KindVideo, Required: true}},
		[]*model.Track{{ID: 2, Kind: model.KindAudio}}, Options{})
	if err == nil || model.CodeOf(err) != model.ErrTrackMapMissing {
		t.Fatalf("err = %v", err)
	}
}

func TestOptionalMissingTargetSkipped(t *testing.T) {
	res, err := Resolve([]Target{
		{Name: "program-video", Kind: model.KindVideo, Required: true},
		{Name: "caption", Kind: model.KindData, Required: false},
	}, []*model.Track{{ID: 1, Kind: model.KindVideo}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Selections) != 1 {
		t.Fatalf("selections = %d", len(res.Selections))
	}
}

func TestTypeMismatchRejected(t *testing.T) {
	id := uint32(1)
	_, err := Resolve([]Target{{Name: "program-audio", Kind: model.KindAudio, Required: true, Explicit: &id}}, tracks(), Options{})
	if err == nil || model.CodeOf(err) != model.ErrTrackMapMismatch {
		t.Fatalf("err = %v", err)
	}
}

func TestExplicitMissingTrackRejected(t *testing.T) {
	id := uint32(99)
	_, err := Resolve([]Target{{Name: "program-video", Kind: model.KindVideo, Required: true, Explicit: &id}}, tracks(), Options{})
	if err == nil || model.CodeOf(err) != model.ErrTrackMapMissing {
		t.Fatalf("err = %v", err)
	}
}

func TestDuplicateSourceTrackIDsRejected(t *testing.T) {
	_, err := Resolve([]Target{{Name: "program-video", Kind: model.KindVideo, Required: true}},
		[]*model.Track{{ID: 1, Kind: model.KindVideo}, {ID: 1, Kind: model.KindVideo}}, Options{})
	if err == nil || model.CodeOf(err) != model.ErrTrackMapDuplicate {
		t.Fatalf("err = %v", err)
	}
}

func TestInvalidInputs(t *testing.T) {
	if _, err := Resolve(nil, tracks(), Options{}); err == nil {
		t.Fatal("no targets should fail")
	}
	if _, err := Resolve([]Target{{Name: "", Kind: model.KindVideo}}, tracks(), Options{}); err == nil {
		t.Fatal("empty name should fail")
	}
	if _, err := Resolve([]Target{{Name: "x", Kind: "subtitle"}}, tracks(), Options{}); err == nil {
		t.Fatal("invalid kind should fail")
	}
}

func TestTargetsFromTracks(t *testing.T) {
	ts := TargetsFromTracks(tracks())
	if len(ts) != 2 || ts[0].Kind != model.KindVideo || ts[1].Kind != model.KindAudio {
		t.Fatalf("targets = %+v", ts)
	}
}
