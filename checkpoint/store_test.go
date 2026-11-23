package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediad/diag"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if _, err := s.Load(); !errors.Is(err, ErrMissing) {
		t.Fatalf("fresh store must report ErrMissing, got %v", err)
	}
	rec := Record{
		Cursors:       map[string]int64{"video": 4096, "sub": 1024},
		LastSegment:   6,
		SegmentHashes: map[int]string{0: "aa", 1: "bb"},
		ManifestHash:  "cc",
	}
	if err := s.Save(rec); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSegment != 6 || got.Cursors["video"] != 4096 || got.Version != 1 ||
		got.SegmentHashes[1] != "bb" || got.ManifestHash != "cc" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestAtomicReplaceLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	for i := 0; i < 5; i++ {
		if err := s.Save(Record{LastSegment: i, Cursors: map[string]int64{}}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temporary file survived: %s", e.Name())
		}
	}
}

func TestCorruptCheckpointDetected(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := s.Load()
	if diag.CodeOf(err) != diag.EResume {
		t.Fatalf("corrupt checkpoint must be E_RESUME, got %v", err)
	}
}

func TestVerifyCommittedSkipsIntactRedoesPartial(t *testing.T) {
	dir := t.TempDir()
	rec := Record{
		Cursors:       map[string]int64{"video": 0},
		LastSegment:   2,
		SegmentHashes: map[int]string{},
	}
	// Write three segment files and record their real hashes.
	for i := 0; i < 3; i++ {
		p := SegmentPath(dir, "video", i)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("payload-"+string(rune('0'+i))), 0o644); err != nil {
			t.Fatal(err)
		}
		h, err := HashFile(p)
		if err != nil {
			t.Fatal(err)
		}
		rec.SegmentHashes[i] = h
	}
	redo, err := VerifyCommitted(dir, rec)
	if err != nil || redo != -1 {
		t.Fatalf("intact output must verify fully, redo=%d err=%v", redo, err)
	}

	// Corrupt segment 1: verification must point the redo at index 1.
	p := SegmentPath(dir, "video", 1)
	if err := os.WriteFile(p, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	redo, err = VerifyCommitted(dir, rec)
	if err != nil || redo != 1 {
		t.Fatalf("tampered segment must force redo from 1, got %d, %v", redo, err)
	}

	// A missing segment is likewise a safe redo point.
	if err := os.Remove(SegmentPath(dir, "video", 2)); err != nil {
		t.Fatal(err)
	}
	redo, _ = VerifyCommitted(dir, rec)
	if redo != 1 { // index 1 still fails first
		t.Fatalf("expected first failure at 1, got %d", redo)
	}
}

func TestClear(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save(Record{LastSegment: 0, Cursors: map[string]int64{}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrMissing) {
		t.Fatalf("after clear Load must report missing, got %v", err)
	}
	if err := s.Clear(); err != nil { // idempotent
		t.Fatalf("clearing twice must be safe, got %v", err)
	}
}
