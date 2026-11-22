package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommitAndResume(t *testing.T) {
	dir := t.TempDir()
	s, rb, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rb != 0 {
		t.Fatalf("rollbacks = %d", rb)
	}
	if err := s.Commit("reorder", []byte("artifact-1")); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, rb, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if rb != 0 || !s2.Done("reorder") {
		t.Fatalf("resume: rb=%d done=%v", rb, s2.Done("reorder"))
	}
	data, err := os.ReadFile(s2.ArtifactPath("reorder"))
	if err != nil || string(data) != "artifact-1" {
		t.Fatalf("artifact = %q, %v", data, err)
	}
	// Idempotent recommit is a no-op.
	if err := s2.Commit("reorder", []byte("different")); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(s2.ArtifactPath("reorder"))
	if string(data) != "artifact-1" {
		t.Fatalf("recommit overwrote artifact: %q", data)
	}
}

func TestCrashBeforeCommitRollsBack(t *testing.T) {
	dir := t.TempDir()
	s, _, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate crash: stage record + artifact written, no commit record.
	if err := s.wal.AppendStage("align", "deadbeef"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ArtifactPath("align"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, rb, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if rb != 1 {
		t.Fatalf("rollbacks = %d, want 1", rb)
	}
	if s2.Done("align") {
		t.Fatal("uncommitted stage must not be done")
	}
	if _, err := os.Stat(s2.ArtifactPath("align")); !os.IsNotExist(err) {
		t.Fatal("uncommitted artifact not rolled back")
	}
}

func TestTruncatedWALTolerated(t *testing.T) {
	dir := t.TempDir()
	s, _, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit("ingest", []byte("x")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Torn tail write: append a partial JSON line.
	f, _ := os.OpenFile(filepath.Join(dir, "wal.log"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"op":"stage","stage":"mani`)
	f.Close()

	s2, _, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if !s2.Recovery().Truncated {
		t.Fatal("truncated tail not detected")
	}
	if !s2.Done("ingest") {
		t.Fatal("committed stage lost after truncation")
	}
}

func TestRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	s, _, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Commit("../escape", []byte("x")); err == nil {
		t.Fatal("path traversal stage accepted")
	}
}
