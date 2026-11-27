package dag

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestJournalRoundTripAndAtomic(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing entry: %v", err)
	}
	e := Entry{Key: "k1", Name: "n1", Status: StatusRunning}
	if err := j.Put(e); err != nil {
		t.Fatal(err)
	}
	// No temp file left behind after the atomic rename.
	if _, err := os.Stat(filepath.Join(dir, "journal.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("temp journal must be renamed away")
	}
	// Reopening reads the persisted state.
	j2, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := j2.Get("k1")
	if err != nil || got.Status != StatusRunning {
		t.Fatalf("reload = %+v err=%v", got, err)
	}
	if !j2.IsDone("nope") {
		// not done yet; now mark succeeded and confirm
	}
	if err := j2.Put(Entry{Key: "k2", Name: "n2", Status: StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	if !j2.IsDone("k2") {
		t.Fatal("succeeded entry must report done")
	}
}

func TestIdempotencyKeyStable(t *testing.T) {
	d := map[string]string{"a": "deadbeef"}
	p := map[string]string{"mode": "fast"}
	k1 := IdempotencyKey("node", d, p)
	k2 := IdempotencyKey("node", map[string]string{"a": "deadbeef"},
		map[string]string{"mode": "fast"})
	if k1 != k2 || len(k1) != 64 {
		t.Fatalf("keys must be stable 64-hex: %q %q", k1, k2)
	}
	// Any input change must change the key.
	if IdempotencyKey("node", map[string]string{"a": "00"}, p) == k1 {
		t.Fatal("different digest must change key")
	}
	if IdempotencyKey("other", d, p) == k1 {
		t.Fatal("different name must change key")
	}
	// Map insertion order must not affect the key.
	kw := IdempotencyKey("n", map[string]string{"z": "1", "a": "2"}, nil)
	kw2 := IdempotencyKey("n", map[string]string{"a": "2", "z": "1"}, nil)
	if kw != kw2 {
		t.Fatal("key must be independent of map iteration order")
	}
}

func TestSnapshotSorted(t *testing.T) {
	dir := t.TempDir()
	j, _ := OpenJournal(dir)
	_ = j.Put(Entry{Key: "ccc", Name: "c", Status: StatusSucceeded})
	_ = j.Put(Entry{Key: "aaa", Name: "a", Status: StatusSucceeded})
	snap := j.Snapshot()
	if snap[0].Key != "aaa" || snap[1].Key != "ccc" {
		t.Fatalf("snapshot not sorted: %+v", snap)
	}
}
