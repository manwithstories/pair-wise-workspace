package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/fault"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "run.chk")
	if err != nil {
		t.Fatal(err)
	}
	want := State{
		Completed:     map[string]bool{"k1": true, "k2": true},
		SegmentHashes: []string{"ab", "cd"},
	}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Completed["k1"] || !got.Completed["k2"] || len(got.Completed) != 2 {
		t.Fatalf("completed=%v", got.Completed)
	}
	if len(got.SegmentHashes) != 2 || got.SegmentHashes[1] != "cd" {
		t.Fatalf("hashes=%v", got.SegmentHashes)
	}
}

func TestMissingFileYieldsEmpty(t *testing.T) {
	s, err := NewStore(t.TempDir(), "none.chk")
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Completed) != 0 {
		t.Fatalf("want empty state, got %+v", st)
	}
}

func TestCorruptedCheckpointDetected(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "run.chk")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(State{Completed: map[string]bool{"k": true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Corrupt(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); fault.KindOf(err) != fault.KindCheckpoint {
		t.Fatalf("want checkpoint fault, got %v", err)
	}
	// Bit-flip in the middle of a valid file must also be caught.
	if err := s.Save(State{Completed: map[string]bool{"k": true}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(s.Path)
	data[len(data)/2] ^= 0xFF
	if err := os.WriteFile(s.Path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); fault.KindOf(err) != fault.KindCheckpoint {
		t.Fatalf("bit-flip not detected: %v", err)
	}
}

func TestPathEscapeRefused(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewStore(dir, filepath.Join("..", "escape.chk")); err == nil {
		t.Fatal("path outside workspace must be refused")
	}
}
