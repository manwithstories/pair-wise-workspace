package track

import (
	"testing"

	"archive/internal/demux"
	"archive/internal/fault"
)

func tr(id uint32, handler, lang, codec string) demux.Track {
	return demux.Track{ID: id, Handler: handler, Lang: lang, Codec: codec}
}

func TestMapDeterministicOrder(t *testing.T) {
	// Deliberately unsorted: audio english, video, audio french.
	tracks := []demux.Track{
		tr(3, "soun", "fra", "mp4a"),
		tr(1, "vide", "und", "avc1"),
		tr(2, "soun", "eng", "mp4a"),
	}
	order, err := Map(tracks)
	if err != nil {
		t.Fatal(err)
	}
	// order[i] is the output slot of tracks[i].
	want := []int{2, 0, 1}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
	// Repeating with a shuffled input yields the same slot assignment
	// for the same tracks.
	again, err := Map([]demux.Track{tracks[1], tracks[2], tracks[0]})
	if err != nil {
		t.Fatal(err)
	}
	if again[0] != 0 || again[1] != 1 || again[2] != 2 {
		t.Fatalf("stable mapping violated: %v", again)
	}
}

func TestMapConflictIsFatal(t *testing.T) {
	tracks := []demux.Track{
		tr(1, "vide", "und", "avc1"),
		tr(2, "vide", "und", "avc1"),
	}
	_, err := Map(tracks)
	if err == nil || fault.LevelOf(err) != fault.Fatal {
		t.Fatalf("expected fatal conflict, got %v", err)
	}
}

func TestMapDifferentCodecIsNotConflict(t *testing.T) {
	if _, err := Map([]demux.Track{
		tr(1, "vide", "und", "avc1"),
		tr(2, "vide", "und", "hvc1"),
	}); err != nil {
		t.Fatalf("distinct codecs must coexist: %v", err)
	}
}

func TestMapEmpty(t *testing.T) {
	if _, err := Map(nil); err == nil {
		t.Fatal("empty track list must error")
	}
}

func TestRole(t *testing.T) {
	cases := map[string]string{"vide": "video", "soun": "audio", "subt": "aux", "": "aux"}
	for handler, want := range cases {
		if got := Role(tr(1, handler, "und", "x")); got != want {
			t.Errorf("Role(%q) = %q, want %q", handler, got, want)
		}
	}
}
