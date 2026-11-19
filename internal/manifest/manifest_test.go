package manifest

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAtomicRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteAtomic(dir, "../escape.json", &Manifest{}); err == nil {
		t.Fatal("path escaping output dir must be rejected")
	}
	if _, err := WriteAtomic(dir, filepath.Join("sub", "..", "..", "x.json"), &Manifest{}); err == nil {
		t.Fatal("nested traversal must be rejected")
	}
}

func TestWriteAtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{Version: 1, InputDigest: "abc123",
		Tracks: []Track{{TrackID: 7, DeviceID: "d", TotalSamples: 99}}}
	p, err := WriteAtomic(dir, "manifest.json", m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p, "manifest.json") {
		t.Fatalf("unexpected path %s", p)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tracks[0].TrackID != 7 || got.InputDigest != "abc123" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}
