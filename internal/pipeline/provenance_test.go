package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"archive/internal/checkpoint"
	"archive/internal/fault"
	"archive/internal/fixture"
)

func TestProvenanceRoundTrip(t *testing.T) {
	root := newArchive(t, fixture.Options{Segments: 2})
	if res := run(t, root, -1); res.Exit != ExitOK {
		t.Fatalf("run = %+v", res)
	}
	cp, err := checkpoint.Open(filepath.Join(root, "work", "checkpoints"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := cp.Entries()
	if err != nil {
		t.Fatal(err)
	}
	p, err := ReadProvenance(root, entries[0])
	if err != nil {
		t.Fatal(err)
	}
	if p.InputSHA256 != entries[0].InputSHA || p.OutputSHA256 != entries[0].OutputSHA {
		t.Fatalf("provenance digests do not match checkpoint: %+v", p)
	}
	if p.ManifestSource != "media.m3u8" || len(p.Tracks) != 2 {
		t.Fatalf("provenance lacks source detail: %+v", p)
	}
	for _, tr := range p.Tracks {
		if tr.Start.Den == 0 || tr.End.Den == 0 || tr.Samples == 0 {
			t.Fatalf("track %+v lacks time coverage", tr)
		}
	}
}

func TestProvenanceMismatchRejected(t *testing.T) {
	root := newArchive(t, fixture.Options{Segments: 1})
	if res := run(t, root, -1); res.Exit != ExitOK {
		t.Fatalf("run = %+v", res)
	}
	cp, _ := checkpoint.Open(filepath.Join(root, "work", "checkpoints"))
	entries, _ := cp.Entries()
	e := entries[0]
	e.OutputSHA = "0" + e.OutputSHA[1:]
	if _, err := ReadProvenance(root, e); err == nil || fault.LevelOf(err) != fault.Fatal {
		t.Fatalf("mismatched checkpoint must be fatal, got %v", err)
	}
	if err := os.Remove(provenancePath(root, e.Index)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadProvenance(root, entries[0]); err == nil {
		t.Fatal("missing provenance must be reported")
	}
}
