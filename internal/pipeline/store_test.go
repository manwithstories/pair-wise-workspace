package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"tsqc/internal/clock"
)

func TestCacheValidatesSourceInputAndOutput(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	key := CacheKey{Source: "fixture-a", InputDigest: "abc", Segment: 0}
	data := []byte("segment-payload")
	if _, err := store.Put(key, data); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.Get(key); !ok || string(got) != string(data) {
		t.Fatalf("miss on exact key: %v", ok)
	}
	if _, ok := store.Get(CacheKey{Source: "fixture-b", InputDigest: "abc", Segment: 0}); ok {
		t.Fatal("source mismatch must miss")
	}
	if _, ok := store.Get(CacheKey{Source: "fixture-a", InputDigest: "abd", Segment: 0}); ok {
		t.Fatal("input mismatch must miss")
	}
	entry := store.entries[manifestKey(key)]
	if err := store.CorruptObject(entry.OutputDigest); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get(key); ok {
		t.Fatal("corrupt object must miss")
	}
	if store.BadHits == 0 {
		t.Fatal("bad hit not counted")
	}
}

func TestCacheEvictionBounded(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := store.Put(CacheKey{Source: "s", InputDigest: "d", Segment: i}, make([]byte, 30)); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.entries) > 3 {
		t.Fatalf("entries=%d", len(store.entries))
	}
	if store.Evicted == 0 {
		t.Fatal("nothing evicted")
	}
}

func TestCheckpointResumeDoesNotDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	cp := &Checkpoint{RunID: "run-1", InputDigest: "in-1"}
	seg := &Segment{Index: 0, StartPTS: 10, EndPTS: 20, Bytes: []byte("abc")}
	if !cp.Commit(seg) {
		t.Fatal("first commit rejected")
	}
	if cp.Commit(seg) {
		t.Fatal("duplicate commit accepted")
	}
	if cp.NextSegment != 1 || len(cp.Segments) != 1 {
		t.Fatalf("cp=%+v", cp)
	}
	if err := cp.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := LoadCheckpoint(path)
	if err != nil || !back.Compatible("run-1", "in-1") {
		t.Fatalf("load=%+v err=%v", back, err)
	}
	if back.Digests()[0] != seg.Digest() {
		t.Fatal("digest not preserved")
	}
	if back.Compatible("run-2", "in-1") || back.Compatible("run-1", "in-2") {
		t.Fatal("identity must be exact")
	}
}

func TestCheckpointCorruptionAndVersionFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")
	os.WriteFile(path, []byte("{not json"), 0o644)
	if cp, err := LoadCheckpoint(path); err == nil || !clock.IsRecoverable(err) {
		t.Fatalf("want recoverable corruption, cp=%v err=%v", cp, err)
	}
	os.WriteFile(path, []byte(`{"version":999}`), 0o644)
	if _, err := LoadCheckpoint(path); !clock.IsCode(err, clock.CodeCheckpoint) {
		t.Fatalf("want version error, got %v", err)
	}
	if cp, err := LoadCheckpoint(filepath.Join(dir, "absent.json")); err != nil || cp != nil {
		t.Fatalf("absent should be clean: %v %v", cp, err)
	}
}
