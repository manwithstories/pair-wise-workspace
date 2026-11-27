package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func sampleEntries() []Entry {
	return []Entry{
		{Camera: "stage", Representation: "primary", Sequence: 1, Start90k: 90000, ContentHash: "h1", FileHash: "f1"},
		{Camera: "stage", Representation: "primary", Sequence: 0, Start90k: 0, ContentHash: "h0", FileHash: "f0"},
		{Camera: "side", Representation: "backup", Sequence: 0, Start90k: 0, ContentHash: "h2", FileHash: "f2", Recovered: true},
	}
}

func TestStoreFlushAndDeterministicHash(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range sampleEntries() {
		s1.Add(e)
	}
	path, err := s1.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("index file missing: %v", err)
	}
	hash1 := s1.Hash()

	// Add the same entries in a different order; canonical hash must match.
	s2, _ := NewStore(filepath.Join(dir, "second"))
	rev := sampleEntries()
	for i := len(rev) - 1; i >= 0; i-- {
		s2.Add(rev[i])
	}
	if got := s2.Hash(); got != hash1 {
		t.Fatalf("hash differs by insertion order: %s vs %s", got, hash1)
	}
	if len(s2.Entries()) != 3 {
		t.Fatal("entries lost")
	}
}

func TestCheckpointAdvanceOnlyAfterValidatedHash(t *testing.T) {
	dir := t.TempDir()
	cm, err := NewCheckpointManager(dir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := cm.Advance("b0", ""); err == nil {
		t.Fatal("advance with empty hash must be rejected")
	}
	if cm.IsComplete("b0") {
		t.Fatal("block marked complete despite rejected advance")
	}
	if err := cm.Advance("b0", "hashA"); err != nil {
		t.Fatal(err)
	}
	// Idempotent re-advance.
	if err := cm.Advance("b0", "hashA"); err != nil {
		t.Fatalf("idempotent advance failed: %v", err)
	}
	if !cm.IsComplete("b0") {
		t.Fatal("b0 should be complete")
	}
}

func TestCheckpointDiskFailureDoesNotAdvance(t *testing.T) {
	dir := t.TempDir()
	cm, _ := NewCheckpointManager(dir, "run-1")
	if err := cm.Advance("b0", "hashA"); err != nil {
		t.Fatal(err)
	}
	cm.FailNext()
	if err := cm.Advance("b1", "hashB"); err == nil {
		t.Fatal("expected simulated disk failure")
	}
	if cm.IsComplete("b1") {
		t.Fatal("checkpoint advanced despite disk failure")
	}
	// The on-disk file must still only contain b0.
	reloaded, err := NewCheckpointManager(dir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	cp, err := reloaded.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cp.CompletedBlocks) != 1 || cp.CompletedBlocks[0] != "b0" {
		t.Fatalf("durable checkpoint corrupted: %+v", cp.CompletedBlocks)
	}
}

func TestCheckpointRejectsCorruptAndStale(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "index")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cpPath := filepath.Join(root, "checkpoint.json")
	if err := os.WriteFile(cpPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	cm, _ := NewCheckpointManager(dir, "run-1")
	if _, err := cm.Load(); err == nil {
		t.Fatal("corrupt checkpoint must be rejected")
	}

	valid := Checkpoint{Version: CheckpointVersion, RunID: "run-1", CompletedBlocks: []string{"b0"}, LastBlock: "b0", IndexHash: "h"}
	writeValid(t, cpPath, valid)
	if _, err := cm.Load(); err != nil {
		t.Fatalf("valid checkpoint rejected: %v", err)
	}

	// A manager with a different run id must reject the stale checkpoint.
	stale, _ := NewCheckpointManager(dir, "run-other")
	if _, err := stale.Load(); err == nil {
		t.Fatal("stale run-id checkpoint must be rejected")
	}
}

func writeValid(t *testing.T, path string, cp Checkpoint) {
	t.Helper()
	data, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
