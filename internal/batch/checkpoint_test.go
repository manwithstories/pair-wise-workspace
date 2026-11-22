package batch

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFreshAndResume(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir, "in", "params", 4, 120, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkChunked(0, 0, 360000); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkChunked(1, 360000, 720000); err != nil {
		t.Fatal(err)
	}
	if m.Done() {
		t.Fatal("should not be done after 2/4 chunks")
	}
	// reopen with resume: same digests, state restored
	m2, err := NewManager(dir, "in", "params", 4, 120, true)
	if err != nil {
		t.Fatal(err)
	}
	snap := m2.Snapshot()
	if len(snap.Completed) != 2 || snap.FirstTick != 0 || snap.LastTick != 720000 {
		t.Fatalf("resumed snapshot = %+v", snap)
	}
	// already-completed chunk cannot advance the checkpoint again
	if err := m2.MarkChunked(1, 360000, 720000); err == nil {
		t.Fatal("duplicate chunk must be rejected")
	}
}

func TestDigestMismatchRejectsResume(t *testing.T) {
	dir := t.TempDir()
	m, _ := NewManager(dir, "in", "params", 3, 100, false)
	if err := m.MarkChunked(0, 0, 1000); err != nil {
		t.Fatal(err)
	}
	// changed input digest must refuse to resume
	if _, err := NewManager(dir, "tampered", "params", 3, 100, true); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
	// changed params digest likewise
	if _, err := NewManager(dir, "in", "other", 3, 100, true); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("params err = %v, want ErrDigestMismatch", err)
	}
	// changed chunk layout is also rejected
	if _, err := NewManager(dir, "in", "params", 4, 75, true); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("layout err = %v, want ErrDigestMismatch", err)
	}
}

func TestResumeWithoutCheckpoint(t *testing.T) {
	if _, err := NewManager(t.TempDir(), "in", "p", 2, 50, true); err == nil {
		t.Fatal("resume with no checkpoint must fail")
	}
}

func TestVerifyAgainstIndex(t *testing.T) {
	dir := t.TempDir()
	m, _ := NewManager(dir, "in", "p", 3, 50, false)
	_ = m.MarkChunked(0, 0, 100)
	_ = m.MarkChunked(1, 100, 200)
	if err := m.VerifyAgainst(map[int]bool{0: true, 1: true}); err != nil {
		t.Fatalf("consistent sets rejected: %v", err)
	}
	// index missing a checkpointed chunk
	if err := m.VerifyAgainst(map[int]bool{0: true}); !errors.Is(err, ErrInconsistent) {
		t.Fatalf("err = %v, want ErrInconsistent", err)
	}
	// index has an un-checkpointed chunk
	if err := m.VerifyAgainst(map[int]bool{0: true, 1: true, 2: true}); !errors.Is(err, ErrInconsistent) {
		t.Fatalf("extra index chunk: %v", err)
	}
}

func TestAtomicWriteLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	m, _ := NewManager(dir, "in", "p", 2, 50, false)
	_ = m.MarkChunked(0, 0, 100)
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("temp checkpoint left: %v", matches)
	}
	if _, err := os.Stat(filepath.Join(dir, fileName)); err != nil {
		t.Fatalf("checkpoint not persisted: %v", err)
	}
}

func TestRejectBadRangesAndChunks(t *testing.T) {
	m, _ := NewManager(t.TempDir(), "in", "p", 2, 50, false)
	if err := m.MarkChunked(0, 100, 50); err == nil {
		t.Fatal("inverted range rejected")
	}
	if err := m.MarkChunked(5, 0, 100); err == nil {
		t.Fatal("out-of-range chunk rejected")
	}
}

func TestDigestStable(t *testing.T) {
	a := Digest([]byte("frame"), []byte("trace"))
	b := Digest([]byte("frame"), []byte("trace"))
	c := Digest([]byte("frame"), []byte("other"))
	if a != b || a == c {
		t.Fatal("digest must be stable and input-sensitive")
	}
}
