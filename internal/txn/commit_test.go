package txn

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"medrelay/internal/journal"
)

func setup(t *testing.T) (*Runner, []Stage, *int) {
	t.Helper()
	root := t.TempDir()
	w, err := journal.Open(filepath.Join(root, "wal.log"), 100)
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	mk := func(name, content string) Stage {
		return Stage{Name: name, Run: func(dir string) error {
			runs++
			return os.WriteFile(filepath.Join(dir, "out.txt"), []byte(content), 0o644)
		}}
	}
	stages := []Stage{mk("a", "1"), mk("b", "2"), mk("c", "3")}
	return &Runner{WAL: w, Root: filepath.Join(root, "stages")}, stages, &runs
}

func TestInterruptAndResume(t *testing.T) {
	r, stages, runs := setup(t)
	r.InterruptAfter = "b"
	if err := r.Execute(stages); !errors.Is(err, ErrInterrupt) {
		t.Fatalf("want ErrInterrupt, got %v", err)
	}
	if *runs != 2 {
		t.Fatalf("ran %d stages, want 2", *runs)
	}
	// b's outputs exist but are uncommitted.
	if _, ok := r.WAL.Committed("b"); ok {
		t.Fatal("b must not be committed")
	}
	r.WAL.Close()

	// Resume with a fresh runner over the same root.
	root := r.Root
	w, err := journal.Open(filepath.Join(filepath.Dir(root), "wal.log"), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	r2 := &Runner{WAL: w, Root: root}
	if err := r2.Execute(stages); err != nil {
		t.Fatal(err)
	}
	// a was committed and skipped; b (uncommitted) rolled back and redone;
	// c ran for the first time.
	if len(r2.Replayed) != 2 || r2.Replayed[0] != "b" || r2.Replayed[1] != "c" {
		t.Fatalf("replayed %v, want [b c]", r2.Replayed)
	}
	if r2.Rollbacks != 1 {
		t.Fatalf("rollbacks %d, want 1 (b's partial output)", r2.Rollbacks)
	}
	if err := r2.Verify(stages); err != nil {
		t.Fatal(err)
	}
}

func TestCommittedOutputsIntactSkipped(t *testing.T) {
	r, stages, runs := setup(t)
	if err := r.Execute(stages); err != nil {
		t.Fatal(err)
	}
	if *runs != 3 {
		t.Fatalf("ran %d, want 3", *runs)
	}
	r.WAL.Close()

	w, _ := journal.Open(filepath.Join(filepath.Dir(r.Root), "wal.log"), 100)
	defer w.Close()
	r2 := &Runner{WAL: w, Root: r.Root}
	if err := r2.Execute(stages); err != nil {
		t.Fatal(err)
	}
	if *runs != 3 {
		t.Fatalf("committed stages must not rerun, runs=%d", *runs)
	}
	if len(r2.Replayed) != 0 || r2.Rollbacks != 0 {
		t.Fatalf("replayed=%v rollbacks=%d", r2.Replayed, r2.Rollbacks)
	}
}

func TestDamagedCommittedOutputRolledBack(t *testing.T) {
	r, stages, _ := setup(t)
	if err := r.Execute(stages); err != nil {
		t.Fatal(err)
	}
	r.WAL.Close()
	// Corrupt a committed stage's output.
	p := filepath.Join(r.Root, "a", "out.txt")
	if err := os.WriteFile(p, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := journal.Open(filepath.Join(filepath.Dir(r.Root), "wal.log"), 100)
	defer w.Close()
	r2 := &Runner{WAL: w, Root: r.Root}
	if err := r2.Execute(stages); err != nil {
		t.Fatal(err)
	}
	if r2.Rollbacks != 1 || len(r2.Replayed) == 0 || r2.Replayed[0] != "a" {
		t.Fatalf("replayed=%v rollbacks=%d", r2.Replayed, r2.Rollbacks)
	}
	if err := r2.Verify(stages); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyDetectsDigestMismatch(t *testing.T) {
	r, stages, _ := setup(t)
	if err := r.Execute(stages); err != nil {
		t.Fatal(err)
	}
	defer r.WAL.Close()
	p := filepath.Join(r.Root, "c", "out.txt")
	if err := os.WriteFile(p, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Verify(stages); err == nil {
		t.Fatal("verify must detect tampering")
	}
}
