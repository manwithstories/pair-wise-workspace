package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

func sampleManifest() *Manifest {
	return &Manifest{
		Version:         1,
		Input:           "in.mov",
		Fingerprint:     "abc",
		SegmentTargetMs: 2000,
		Segments: []SegmentEntry{
			{Index: 0, StartMs: 0, EndMs: 2000, KeyframeSample: 0},
			{Index: 1, StartMs: 2000, EndMs: 4000, KeyframeSample: 60},
		},
		Representations: []Representation{
			{ID: "rep0", TrackID: 1, TargetBitrate: 450000, ActualBitrate: 457496},
		},
	}
}

func TestWriteLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := sampleManifest()
	if err := Write(dir, m); err != nil {
		t.Fatal(err)
	}
	if m.Digest == "" {
		t.Fatal("digest not computed")
	}
	// Completion marker exists, no tmp file remains.
	if _, err := os.Stat(filepath.Join(dir, CompleteName)); err != nil {
		t.Fatal("completion marker missing")
	}
	if _, err := os.Stat(filepath.Join(dir, Name+".tmp")); !os.IsNotExist(err) {
		t.Fatal("tmp file survived atomic write")
	}
	loaded, err := Load(filepath.Join(dir, Name))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Digest != m.Digest {
		t.Fatal("digest changed after round trip")
	}
	if len(loaded.Segments) != 2 {
		t.Fatalf("segments: %d", len(loaded.Segments))
	}
}

func TestDigestDeterministic(t *testing.T) {
	a, b := sampleManifest(), sampleManifest()
	if a.ComputeDigest() != b.ComputeDigest() {
		t.Fatal("identical manifests must have identical digests")
	}
	b.Segments[0].EndMs = 2001
	if a.ComputeDigest() == b.ComputeDigest() {
		t.Fatal("digest must change with content")
	}
}

func TestCleanRemovesUncommitted(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, Name+".tmp"), []byte("junk"), 0o644)
	os.WriteFile(filepath.Join(dir, CompleteName), []byte("old"), 0o644)
	os.WriteFile(filepath.Join(dir, Name), []byte("{}"), 0o644)
	if err := Clean(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, Name+".tmp")); !os.IsNotExist(err) {
		t.Fatal("tmp not cleaned")
	}
	if _, err := os.Stat(filepath.Join(dir, CompleteName)); !os.IsNotExist(err) {
		t.Fatal("stale marker not cleaned")
	}
	// The committed manifest itself is never removed by Clean.
	if _, err := os.Stat(filepath.Join(dir, Name)); err != nil {
		t.Fatal("committed manifest removed")
	}
}
