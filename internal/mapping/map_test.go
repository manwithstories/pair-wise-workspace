package mapping

import (
	"testing"

	"archive/internal/mp4"
)

func TestMapOrdersByTypeLanguageAndID(t *testing.T) {
	tracks := []mp4.Track{
		{ID: 9, Type: "audio"}, // untagged: falls back to ID order
		{ID: 3, Type: "audio", Language: "jpn"},
		{ID: 1, Type: "video", Language: "eng"},
		{ID: 2, Type: "audio", Language: "eng"},
		{ID: 7, Type: "audio"}, // untagged, lower ID than 9
		{ID: 4, Type: "subtitle", Language: "chi"},
	}
	got := Map(tracks)
	want := []MappedTrack{
		{OutputIndex: 0, Type: "video", Language: "eng", SourceID: 1},
		{OutputIndex: 0, Type: "audio", Language: "eng", SourceID: 2},
		{OutputIndex: 1, Type: "audio", Language: "jpn", SourceID: 3},
		{OutputIndex: 2, Type: "audio", Language: "", SourceID: 7},
		{OutputIndex: 3, Type: "audio", Language: "", SourceID: 9},
		{OutputIndex: 0, Type: "subtitle", Language: "chi", SourceID: 4},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("idx %d: got %+v want %+v", i, got[i], want[i])
		}
	}
	if got[2].Key() != "audio:jpn#1" {
		t.Fatalf("key %q", got[2].Key())
	}
}

func TestMapDeterministic(t *testing.T) {
	tracks := []mp4.Track{
		{ID: 5, Type: "audio"},
		{ID: 2, Type: "audio"},
		{ID: 8, Type: "audio"},
	}
	a, b := Map(tracks), Map(tracks)
	for i := range a {
		if a[i] != b[i] || a[i].SourceID != []uint32{2, 5, 8}[i] {
			t.Fatalf("unstable mapping %+v", a)
		}
	}
}
