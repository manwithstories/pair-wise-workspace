package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteAndIdempotentCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")
	s, err := Load(path, "case")
	if err != nil {
		t.Fatal(err)
	}
	dup, merr := s.Commit(Chunk{ID: "c1", Index: 0, SHA256: "abc", Bytes: 3})
	if merr != nil || dup {
		t.Fatalf("first commit dup=%v err=%v", dup, merr)
	}
	dup, merr = s.Commit(Chunk{ID: "c1", Index: 0, SHA256: "abc", Bytes: 3})
	if merr != nil || !dup {
		t.Fatalf("matching replay should be duplicate no-op: dup=%v err=%v", dup, merr)
	}
	if len(s.Order()) != 1 {
		t.Fatalf("order=%v", s.Order())
	}

	reloaded, lerr := Load(path, "case")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if !reloaded.Has("c1") || len(reloaded.Order()) != 1 {
		t.Fatalf("checkpoint did not reload: %+v", reloaded.data)
	}
	_, merr = reloaded.Commit(Chunk{ID: "c1", Index: 0, SHA256: "different"})
	if merr == nil {
		t.Fatal("conflicting chunk hash must be rejected")
	}
}

func TestWriteFileAtomicCreatesParents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deep", "file.bin")
	if err := WriteFileAtomic(path, []byte("mediaqc"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "mediaqc" {
		t.Fatalf("read=%q err=%v", got, err)
	}
}
