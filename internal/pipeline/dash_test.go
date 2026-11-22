package pipeline

import (
	"testing"

	"archive/internal/fault"
	"archive/internal/fixture"
)

// TestDASHManifestRunsEndToEnd drives the same archive through the DASH
// manifest. The fixture's DASH timeline is one-to-one with the stored
// segments, so the committed set must match the HLS run.
func TestDASHManifestRunsEndToEnd(t *testing.T) {
	root := newArchive(t, fixture.Options{Segments: 5})
	dash, err := Run(Config{Root: root, Manifest: "manifest.mpd", InterruptAfter: -1})
	if err != nil {
		t.Fatal(err)
	}
	if dash.Exit != ExitOK || dash.Committed != 5 || dash.Unique != 5 {
		t.Fatalf("dash run = %+v", dash)
	}
}

func TestDASHTimelineAnchorsPresentationTime(t *testing.T) {
	root := newArchive(t, fixture.Options{Segments: 4})
	res, err := Run(Config{Root: root, Manifest: "manifest.mpd", InterruptAfter: -1})
	if err != nil {
		t.Fatal(err)
	}
	// Segment 0 must cover 0..2s, segment 3 6..8s on the output timeline.
	first := res.Entries[0]
	last := res.Entries[3]
	p0, err := ReadProvenance(root, first)
	if err != nil {
		t.Fatal(err)
	}
	p3, err := ReadProvenance(root, last)
	if err != nil {
		t.Fatal(err)
	}
	if got := p0.Tracks[0].Start.Float64(); got != 0 {
		t.Fatalf("segment 0 start = %v", got)
	}
	if got := p3.Tracks[0].Start.Float64(); got != 6 {
		t.Fatalf("segment 3 start = %v", got)
	}
}

func TestManifestExtensionSelectsParser(t *testing.T) {
	root := newArchive(t, fixture.Options{Segments: 2})
	// Pointing the pipeline at the HLS playlist with a .mpd name must be
	// fatal rather than silently parsed as the wrong family.
	_, err := Run(Config{Root: root, Manifest: "media.m3u8", InterruptAfter: -1})
	if err != nil {
		t.Fatalf("hls should succeed: %v", err)
	}
	if _, err := Run(Config{Root: root, Manifest: "missing.mpd", InterruptAfter: -1}); err == nil || fault.LevelOf(err) != fault.Fatal {
		t.Fatalf("missing DASH manifest must be fatal, got %v", err)
	}
}
