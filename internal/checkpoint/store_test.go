package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/archive/tsrestore/internal/timeline"
)

func secret() []byte { return []byte("run-secret-41") }

func TestCommitAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ckpt.json")
	s, err := Open(path, secret())
	if err != nil {
		t.Fatal(err)
	}
	s.SetMeta(41, 360)
	if err := s.CommitSegment("v1", 0, "hash0", 90000*6, 100); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitSegment("v1", 1, "hash1", 90000*12, 200); err != nil {
		t.Fatal(err)
	}
	if got := s.NextIndex("v1"); got != 2 {
		t.Fatalf("next index = %d, want 2", got)
	}
	if s.HashFor("v1", 0) != "hash0" {
		t.Fatal("committed hash not retrievable")
	}

	// Reopen: progress must survive and verify.
	s2, err := Open(path, secret())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if s2.NextIndex("v1") != 2 || s2.HashFor("v1", 1) != "hash1" {
		t.Fatalf("reloaded progress wrong: %+v", s2.Snapshot())
	}
	if s2.Snapshot().Seed != 41 {
		t.Fatal("run metadata lost across reload")
	}
}

func TestIdempotentRecommitAndHashConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ckpt.json")
	s, _ := Open(path, secret())
	if err := s.CommitSegment("v1", 0, "same", 0, 1); err != nil {
		t.Fatal(err)
	}
	// Same index + same hash is idempotent and must not advance twice.
	if err := s.CommitSegment("v1", 0, "same", 0, 1); err != nil {
		t.Fatalf("identical recommit should be allowed: %v", err)
	}
	if s.NextIndex("v1") != 1 {
		t.Fatal("idempotent recommit advanced the cursor")
	}
	// Same index with a different hash is an integrity failure.
	err := s.CommitSegment("v1", 0, "different", 0, 1)
	if !errors.Is(err, timeline.ErrIntegrity) {
		t.Fatalf("hash conflict must be ErrIntegrity, got %v", err)
	}
}

func TestTamperedCheckpointRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ckpt.json")
	s, _ := Open(path, secret())
	if err := s.CommitSegment("v1", 0, "h0", 0, 1); err != nil {
		t.Fatal(err)
	}
	// Flip a byte inside the stored JSON without recomputing the checksum.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	idx := -1
	for i, b := range raw {
		if b == 'h' && i+1 < len(raw) && raw[i+1] == '0' {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("could not locate hash field to tamper")
	}
	raw[idx] = 'x'
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, secret()); !errors.Is(err, timeline.ErrIntegrity) {
		t.Fatalf("tampered checkpoint must be rejected, got %v", err)
	}
}

func TestWrongSecretRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ckpt.json")
	s, _ := Open(path, secret())
	if err := s.CommitSegment("v1", 0, "h0", 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, []byte("other-secret")); !errors.Is(err, timeline.ErrIntegrity) {
		t.Fatalf("checkpoint under wrong secret must be rejected, got %v", err)
	}
}
