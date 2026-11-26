package recovery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIndexRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx")
	x, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	pd := ParamsDigest("abc", 32)
	if err := x.Append(&Record{Type: "header", Version: IndexVersion, ParamsDigest: pd, InputDigest: "abc", ChunkSize: 32}); err != nil {
		t.Fatal(err)
	}
	cp := NewCheckpointer(x)
	if cp.Run() != 1 {
		t.Fatalf("run=%d", cp.Run())
	}
	if err := cp.Commit(Record{Chunk: 1, FirstSeq: 1, LastSeq: 32, OutDigest: "d1", Segments: 32}); err != nil {
		t.Fatal(err)
	}
	if err := cp.Finish("rd"); err != nil {
		t.Fatal(err)
	}
	x.Close()

	y, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer y.Close()
	if y.Header == nil || y.Header.ParamsDigest != pd {
		t.Fatalf("header=%+v", y.Header)
	}
	if !NewCheckpointer(y).Committed(1) {
		t.Fatal("chunk 1 should be committed")
	}
	if y.Done == nil || y.Done.ResultsDigest != "rd" {
		t.Fatalf("done=%+v", y.Done)
	}
	if y.CorruptTail {
		t.Fatal("unexpected corrupt tail")
	}
}

func TestIndexCorruptTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx")
	x, _ := Open(path)
	cp := NewCheckpointer(x)
	cp.Commit(Record{Chunk: 1, OutDigest: "d1", Segments: 3})
	x.Close()
	// simulate a torn write at the tail
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("deadbeef {not json")
	f.Close()

	y, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer y.Close()
	if !y.CorruptTail {
		t.Fatal("corrupt tail not detected")
	}
	if !NewCheckpointer(y).Committed(1) {
		t.Fatal("valid prefix must survive")
	}
	st, _ := os.Stat(path)
	if st.Size() == 0 || st.Size() > 200 {
		t.Fatalf("tail not truncated: %d", st.Size())
	}
}

func TestCheckpointerIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx")
	x, _ := Open(path)
	defer x.Close()
	cp := NewCheckpointer(x)
	rec := Record{Chunk: 2, OutDigest: "d", Segments: 5}
	if err := cp.Commit(rec); err != nil {
		t.Fatal(err)
	}
	if err := cp.Commit(rec); err != nil {
		t.Fatal("identical re-commit must be a no-op")
	}
	if len(x.Chunks) != 1 {
		t.Fatalf("chunks=%d", len(x.Chunks))
	}
	if err := cp.Commit(Record{Chunk: 2, OutDigest: "other", Segments: 5}); err == nil {
		t.Fatal("conflicting re-commit must fail")
	}
	if x.ResumedChunks() != 0 {
		t.Fatal("first run must not count as resumed")
	}
}

func TestRunNumbersIncrement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx")
	x, _ := Open(path)
	NewCheckpointer(x).Commit(Record{Chunk: 1, OutDigest: "a", Segments: 1})
	x.Close()
	y, _ := Open(path)
	defer y.Close()
	cp2 := NewCheckpointer(y)
	if cp2.Run() != 2 {
		t.Fatalf("run=%d", cp2.Run())
	}
	cp2.Commit(Record{Chunk: 2, OutDigest: "b", Segments: 1})
	if y.ResumedChunks() != 1 {
		t.Fatalf("resumed=%d", y.ResumedChunks())
	}
}
