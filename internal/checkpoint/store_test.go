package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"mediad/internal/fault"
	segmenthash "mediad/internal/hash"
)

func hexDigest(c byte) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = c
	}
	return string(out)
}

func rec(uri string) segmenthash.Record {
	return segmenthash.Record{URI: uri, Sequence: 1, Role: "output", Size: 1, SHA256: segmenthash.Bytes([]byte(uri))}
}

func TestChunkPlan(t *testing.T) {
	if got := ChunkCount(20, 8); got != 3 {
		t.Fatalf("chunks = %d", got)
	}
	if got := ChunkCount(0, 8); got != 0 {
		t.Fatalf("empty chunks = %d", got)
	}
	start, end := ChunkRange(2, 20, 8)
	if start != 16 || end != 20 {
		t.Fatalf("range = %d,%d", start, end)
	}
	start, end = ChunkRange(5, 20, 8)
	if start != 20 || end != 20 {
		t.Fatalf("past-end range = %d,%d", start, end)
	}
	if Validate(0) == nil || Validate(-1) == nil || Validate(1_000_000) == nil {
		t.Fatal("invalid chunk sizes accepted")
	}
}

func TestSaveLoadResume(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := store.Load(); st != nil || err != nil {
		t.Fatalf("first load = %v %v", st, err)
	}
	st, err := NewState(hexDigest('a'), 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddChunk(Chunk{Index: 1, Segments: []segmenthash.Record{rec("b.m4s")}}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddChunk(Chunk{Index: 0, Segments: []segmenthash.Record{rec("a.m4s")}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil || loaded == nil {
		t.Fatalf("load = %v %v", loaded, err)
	}
	if loaded.CompletedSegments() != 2 || len(loaded.Chunks) != 2 || loaded.Chunks[0].Index != 0 {
		t.Fatalf("loaded = %+v", loaded)
	}
	done, err := Resume(loaded, hexDigest('a'))
	if err != nil || len(done) != 2 || done[0] != 0 || done[1] != 1 {
		t.Fatalf("resume = %v %v", done, err)
	}
	if _, err := Resume(loaded, hexDigest('b')); !errors.Is(err, ErrInputChanged) {
		t.Fatalf("expected input changed, got %v", err)
	}
	if done, err := Resume(nil, hexDigest('a')); err != nil || done != nil {
		t.Fatalf("nil resume = %v %v", done, err)
	}
}

func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	store, _ := New(dir)
	st, _ := NewState(hexDigest('c'), 4)
	for i := 0; i < 5; i++ {
		st.AddChunk(Chunk{Index: i, Segments: []segmenthash.Record{rec("s")}})
		if err := store.Save(st); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "checkpoint.json" {
			t.Fatalf("unexpected file %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "checkpoint.json")); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptCheckpointRejected(t *testing.T) {
	dir := t.TempDir()
	store, _ := New(dir)
	cases := []string{
		"not json",
		`{"version":99,"manifest_sha256":"` + hexDigest('a') + `","chunk_size":8}`,
		`{"version":1,"manifest_sha256":"short","chunk_size":8}`,
		`{"version":1,"manifest_sha256":"` + hexDigest('a') + `","chunk_size":0}`,
		`{"version":1,"manifest_sha256":"` + hexDigest('a') + `","chunk_size":8,"chunks":[{"index":1},{"index":1}]}`,
		`{"unknown":true}`,
	}
	for i, body := range cases {
		if err := os.WriteFile(store.Path(), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("case %d: expected corrupt, got %v", i, err)
		}
	}
}

func TestNewStateValidation(t *testing.T) {
	if _, err := NewState("xyz", 8); err == nil {
		t.Fatal("short digest accepted")
	}
	if _, err := NewState(hexDigest('A'), 8); err == nil {
		t.Fatal("uppercase digest accepted")
	}
	if _, err := NewState(hexDigest('a'), 0); err == nil {
		t.Fatal("zero chunk size accepted")
	}
}

func TestFaultsAndDuplicateChunk(t *testing.T) {
	st, _ := NewState(hexDigest('d'), 4)
	f := fault.MustNew(fault.KindBoxOverflow, fault.Location{Segment: "a", Offset: 8}, "x", true, false)
	if err := st.AddChunk(Chunk{Index: 0, Faults: []fault.Fault{f}}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddChunk(Chunk{Index: 0, Faults: []fault.Fault{f}}); err != nil {
		t.Fatal(err)
	}
	if len(st.Chunks) != 1 || len(st.AllFaults()) != 1 {
		t.Fatalf("duplicate chunk not replaced: %+v", st)
	}
}

func TestConcurrentSavesStayConsistent(t *testing.T) {
	dir := t.TempDir()
	store, _ := New(dir)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := NewState(hexDigest('e'), 4)
			if err != nil {
				t.Error(err)
				return
			}
			st.AddChunk(Chunk{Index: i, Segments: []segmenthash.Record{rec("s")}})
			if err := store.Save(st); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("final load: %v", err)
	}
	if len(loaded.Chunks) != 1 {
		t.Fatalf("chunks = %d", len(loaded.Chunks))
	}
}
