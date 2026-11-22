package scan

import (
	"os"
	"path/filepath"
	"testing"

	"mediaslice/media"
)

func TestIndexContainsRequiredFingerprintFields(t *testing.T) {
	dir := t.TempDir()
	mp4Path, rtpPath, _ := buildFixture(t, dir, "800k", false)
	idx, err := ScanFile(mp4Path, rtpPath, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.InputSHA256) != 64 {
		t.Fatalf("input sha256 len=%d", len(idx.InputSHA256))
	}
	if idx.ParserVersion != ParserVersion {
		t.Fatal("parser version missing")
	}
	if len(idx.Summaries) != len(idx.Tracks) {
		t.Fatal("track summaries missing")
	}
	for _, sm := range idx.Summaries {
		if len(sm.SampleDigest) != 64 || sm.SampleCount == 0 {
			t.Fatalf("bad summary %+v", sm)
		}
	}
	path, err := WriteIndex(dir, idx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	// Version bump invalidates the index.
	idx.ParserVersion = "old"
	mm := VerifyIndex(idx, mp4Path)
	if mm == nil || mm.Field != "parser_version" {
		t.Fatalf("version change not detected: %v", mm)
	}
}

func TestEmptyEditSegmentDiagnostic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "emptyedit.mp4")
	// Reuse the negative-edit helper then rewrite elst duration to zero by
	// constructing a dedicated file via the fixture box writer.
	writeZeroDurationEditMP4(t, path)
	idx, err := ScanFile(path, "", DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if e := idx.Errors.First(media.KindEditList); e == nil {
		t.Fatalf("zero-duration edit not flagged: %v", idx.Errors)
	}
}
