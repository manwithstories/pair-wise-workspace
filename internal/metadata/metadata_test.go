package metadata

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteAndProbeMinimalMP4(t *testing.T) {
	spec := VideoTrackSpec{
		TrackID: 1, Timescale: 24000, Width: 320, Height: 240, Duration: 96000,
	}
	raw, err := Marshal(spec, 240)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := Probe(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if summary.Timescale != 24000 || summary.Duration != 96000 {
		t.Fatalf("movie header wrong: %+v", summary)
	}
	if !summary.FastStart {
		t.Fatal("moov precedes mdat: FastStart must be true")
	}
	if len(summary.VideoTracks) != 1 {
		t.Fatalf("video tracks=%d", len(summary.VideoTracks))
	}
	v := summary.VideoTracks[0]
	if v.TrackID != 1 || v.Width != 320 || v.Height != 240 {
		t.Fatalf("video track meta wrong: %+v", v)
	}
	if v.Profile != 66 || v.Level != 30 {
		t.Fatalf("AVC profile/level wrong: %+v", v)
	}
}

func TestProbeRejectsGarbage(t *testing.T) {
	if _, err := Probe(bytes.NewReader([]byte("not an mp4 at all"))); err == nil ||
		!strings.Contains(err.Error(), "mp4 probe") {
		t.Fatalf("want wrapped probe error, got %v", err)
	}
}

func TestDeterministicMarshal(t *testing.T) {
	spec := VideoTrackSpec{TrackID: 2, Timescale: 90000, Width: 64, Height: 64, Duration: 3000}
	a, err := Marshal(spec, 10)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Marshal(spec, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("MP4 fixture generation must be deterministic")
	}
}
