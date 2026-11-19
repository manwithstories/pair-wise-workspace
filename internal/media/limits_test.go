package media

import "testing"

func TestBuildRejectsTooManyTracks(t *testing.T) {
	tracks := make([]TrackSpec, MaxTracks+1)
	if _, err := BuildMP4(tracks); err == nil {
		t.Fatal("too many tracks accepted")
	}
}

func TestBuildRejectsNoTracks(t *testing.T) {
	if _, err := BuildMP4(nil); err == nil {
		t.Fatal("empty track list accepted")
	}
}
