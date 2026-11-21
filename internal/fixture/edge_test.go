package fixture

import (
	"strings"
	"testing"

	"mediad/internal/model"
	"mediad/internal/probe"
)

func TestOutOfRangeCropNamesTrackAndTimeRange(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/bad.mp4"
	spec := MP4Spec{Tracks: []TrackSpec{{
		ID: 7, Kind: "video", Codec: "avc1", Timescale: 30000,
		Frames: 30, FrameDelta: 1000, Width: 64, Height: 64, SampleBytes: 16,
		CropLeft: 40, CropRight: 40,
	}}}
	if _, err := WriteMP4(path, spec, 3); err != nil {
		t.Fatal(err)
	}
	_, err := probe.MP4(path, probe.DefaultLimits())
	if err == nil {
		t.Fatal("expected crop error")
	}
	if model.KindOf(err) != model.KindInvalidMedia {
		t.Fatalf("kind=%v", model.KindOf(err))
	}
	e, _ := model.AsError(err)
	if e.TrackID != 7 {
		t.Fatalf("track=%d", e.TrackID)
	}
	if !strings.Contains(e.Detail, "[0, 1]") {
		t.Fatalf("detail %q should name the time range [0, 1]", e.Detail)
	}
}
