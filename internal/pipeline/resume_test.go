package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mediacore/internal/checkpoint"
)

// A rerun against the same checkpoint must converge to the same absolute
// committed boundary rather than accumulate already-committed samples.
func TestResumeNoDoubleCount(t *testing.T) {
	dir := t.TempDir()
	ckpt := filepath.Join(dir, "ck.json")
	cfg := func() Config {
		c := DefaultConfig(42, 4, ckpt, filepath.Join(dir, "o.mp4"))
		return c
	}

	first, err := Run(context.Background(), cfg())
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.NewStore(ckpt)
	if err != nil {
		t.Fatal(err)
	}
	st1, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}

	second, err := Run(context.Background(), cfg())
	if err != nil {
		t.Fatal(err)
	}
	st2, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}

	if st2.CommittedSamples != st1.CommittedSamples {
		t.Fatalf("committed samples accumulated on resume: %d -> %d",
			st1.CommittedSamples, st2.CommittedSamples)
	}
	if st2.CommittedInputSeq != st1.CommittedInputSeq {
		t.Fatalf("committed input seq changed on resume: %d -> %d",
			st1.CommittedInputSeq, st2.CommittedInputSeq)
	}
	if first.Samples != second.Samples {
		t.Fatalf("logical output size changed across rerun: %d vs %d",
			first.Samples, second.Samples)
	}
	if first.SHA256 != second.SHA256 {
		t.Fatal("output checksum must be stable across a resumed rerun")
	}
}

// A corrupt checkpoint rolls back to the start without losing the run.
func TestCorruptCheckpointRollbackSafe(t *testing.T) {
	dir := t.TempDir()
	ckpt := filepath.Join(dir, "ck.json")
	if err := os.WriteFile(ckpt, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig(42, 2, ckpt, filepath.Join(dir, "o.mp4"))
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("corrupt checkpoint must roll back, not fail: %v", err)
	}
	if res.Status != "ok" || res.Samples == 0 {
		t.Fatalf("rollback run produced no output: %+v", res)
	}
}
