package checkpoint

import (
	"path/filepath"
	"testing"

	"mediadeliver/internal/model"
)

func digest(manifest string, sources map[string]string) model.InputDigest {
	return model.InputDigest{ManifestSHA256: manifest, SourceSHA256: sources}
}

func sampleSeg(name string) *model.SegmentRecord {
	return &model.SegmentRecord{
		Index: 0, OutputFile: name, PacketCount: 4, Bytes: 40,
		SHA256Hex: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Ranges: []model.SourceRange{
			{MaterialID: "cam1", TrackID: 1, FirstSeq: 0, LastSeq: 3, Packets: 4},
		},
	}
}

func TestFreshRunThenResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.json")
	s := NewStore(path)
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	d := digest("m1", map[string]string{"cam1": "s1"})
	if _, err := s.EnsureInput(d); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(sampleSeg("seg0")); err != nil {
		t.Fatal(err)
	}
	if !s.IsComplete("seg0") {
		t.Fatal("committed segment must report complete")
	}
	if st.Version != 1 {
		t.Fatalf("version: %d", st.Version)
	}

	// Rerun: reload from disk and present the same digest.
	s2 := NewStore(path)
	if _, err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.EnsureInput(d); err != nil {
		t.Fatalf("matching digest must allow resume: %v", err)
	}
	if !s2.IsComplete("seg0") {
		t.Fatal("resumed state must remember completed segment")
	}
	snap := s2.Snapshot()
	if len(snap.Order) != 1 || snap.Order[0] != "seg0" {
		t.Fatalf("order not persisted: %v", snap.Order)
	}
}

func TestDigestChangeRejectsResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.json")
	s := NewStore(path)
	if _, err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureInput(digest("m1", map[string]string{"cam1": "s1"})); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(sampleSeg("seg0")); err != nil {
		t.Fatal(err)
	}

	s2 := NewStore(path)
	if _, err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	// Manifest changed.
	_, err := s2.EnsureInput(digest("m2", map[string]string{"cam1": "s1"}))
	if model.KindOf(err) != model.KindConflict {
		t.Fatalf("manifest digest change must be KindConflict, got %v", err)
	}
	// Source changed.
	s3 := NewStore(path)
	_, _ = s3.Load()
	_, err = s3.EnsureInput(digest("m1", map[string]string{"cam1": "s9"}))
	if model.KindOf(err) != model.KindConflict {
		t.Fatalf("source digest change must be KindConflict, got %v", err)
	}
}

func TestDuplicateCommitRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.json")
	s := NewStore(path)
	_, _ = s.Load()
	_, _ = s.EnsureInput(digest("m1", nil))
	if err := s.Commit(sampleSeg("seg0")); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(sampleSeg("seg0")); model.KindOf(err) != model.KindConflict {
		t.Fatalf("duplicate commit must be KindConflict, got %v", err)
	}
}

func TestCorruptCheckpointNotResumable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cp.json")
	if err := writeFile(path, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path)
	_, err := s.Load()
	if model.KindOf(err) != model.KindCorruptMedia {
		t.Fatalf("corrupt checkpoint must be KindCorruptMedia, got %v", err)
	}
}

func TestDiscardLeavesNoResumableState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.json")
	s := NewStore(path)
	_, _ = s.Load()
	_, _ = s.EnsureInput(digest("m1", nil))
	_ = s.Commit(sampleSeg("seg0"))
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}
	s2 := NewStore(path)
	st, err := s2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Completed) != 0 {
		t.Fatal("discard must leave no completed segments")
	}
}

func TestNoPartialTmpLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cp.json")
	s := NewStore(path)
	_, _ = s.Load()
	_, _ = s.EnsureInput(digest("m1", nil))
	_ = s.Commit(sampleSeg("seg0"))
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("atomic write left temp files: %v", matches)
	}
}
