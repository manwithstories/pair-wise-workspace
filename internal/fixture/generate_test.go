package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"mediacore/internal/scan"
)

func mustBuild(t *testing.T, opt Options) Manifest {
	t.Helper()
	opt.Dir = t.TempDir()
	m, err := Build(opt)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return m
}

func TestGeneratedFixtureIsWellFormed(t *testing.T) {
	m := mustBuild(t, Options{Name: "sample", Segments: 4})
	if m.Size == 0 || len(m.SHA256) != 64 {
		t.Fatalf("manifest: %+v", m)
	}
	sc, err := scan.Run(m.Path, scan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var video, audio, sub int
	for _, tr := range sc.Tracks {
		switch tr.Kind {
		case scan.TrackVideo:
			video++
		case scan.TrackAudio:
			audio++
		case scan.TrackSubtitle:
			sub++
		}
	}
	if video != 1 || audio != 1 {
		t.Fatalf("tracks: video=%d audio=%d", video, audio)
	}
	if len(sc.Chapters) == 0 {
		t.Fatal("no chapters generated")
	}
	if len(sc.Keyframes) < 3 {
		t.Fatalf("only %d keyframes", len(sc.Keyframes))
	}
	if audioTrack := sc.Tracks[1]; audioTrack.SampleRate != 44100 {
		t.Fatalf("fixture audio should be 44.1kHz to force resampling, got %d", audioTrack.SampleRate)
	}
}

func TestGeneratedFixtureSubtitlesAndQuarantine(t *testing.T) {
	m := mustBuild(t, Options{Name: "cues", Segments: 4})
	sc, err := scan.Run(m.Path, scan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var cues int
	for _, rec := range sc.Meta {
		if rec.Kind == "subtitle" && !rec.Quarantined {
			cues++
		}
	}
	if cues == 0 {
		t.Fatal("no subtitle cues scanned")
	}
}

func TestTruncatedFixtureIsolatesRecords(t *testing.T) {
	m := mustBuild(t, Options{Name: "trunc", Segments: 4, Fault: FaultTruncate})
	sc, err := scan.Run(m.Path, scan.Options{})
	if err != nil {
		// A truncated MP4 may fail to open at all; that is also an isolated
		// corrupt input, but it must be reported rather than panicking.
		t.Logf("scan of truncated fixture: %v", err)
		return
	}
	_, quarantined := sc.Counts()
	if quarantined == 0 {
		t.Fatal("truncated fixture produced no quarantined records")
	}
	for _, rec := range sc.Meta {
		if rec.Quarantined && rec.Reason == "" {
			t.Fatal("quarantined record without a reason")
		}
	}
}

func TestManifestDigestMatchesFile(t *testing.T) {
	m := mustBuild(t, Options{Name: "digest", Segments: 2})
	dg, size, err := DigestFile(m.Path)
	if err != nil {
		t.Fatal(err)
	}
	if dg != m.SHA256 || size != m.Size {
		t.Fatalf("manifest digest %s/%d != file %s/%d", m.SHA256, m.Size, dg, size)
	}
}

func TestOptionsValidation(t *testing.T) {
	dir := t.TempDir()
	if _, err := (Options{Dir: dir, Segments: 1000}).Validate(); err == nil {
		t.Fatal("oversize segment count accepted")
	}
	if _, err := (Options{Dir: dir, Format: "avi"}).Validate(); err == nil {
		t.Fatal("unsupported format accepted")
	}
	if _, err := (Options{Dir: dir, Fault: "nope"}).Validate(); err == nil {
		t.Fatal("unknown fault accepted")
	}
	if _, err := (Options{}).Validate(); err == nil {
		t.Fatal("missing dir accepted")
	}
	opt, err := (Options{Dir: dir}).Validate()
	if err != nil {
		t.Fatal(err)
	}
	if opt.Segments != 8 || opt.Format != FormatMP4 || opt.MaxRecordBytes != DefaultMaxRecordBytes {
		t.Fatalf("defaults: %+v", opt)
	}
}

func TestGeneratedFixtureIsReproducible(t *testing.T) {
	// x264 and aac are deterministic for fixed inputs and settings, so two
	// builds of the same options must yield the same bytes.
	dir := t.TempDir()
	a, err := Build(Options{Dir: dir, Name: "a", Segments: 2})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(Options{Dir: dir, Name: "b", Segments: 2})
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 != b.SHA256 {
		t.Fatalf("fixture not reproducible: %s vs %s", a.SHA256, b.SHA256)
	}
	// Clean up the generated media; only manifests belong in the repo.
	os.Remove(a.Path)
	os.Remove(b.Path)
	if _, err := os.Stat(filepath.Dir(a.Path)); err != nil {
		t.Fatal(err)
	}
}
