package store

import (
	"os"
	"path/filepath"
	"testing"

	"mediacore/internal/clock"
)

func TestCommitLoadRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := Key{Node: "cut", InputDigest: "in", ParamsDigest: "p", Version: "1.0.0"}.IdempotencyKey()
	written, err := s.Commit("cut", key, []byte(`{"a":1}`))
	if err != nil || !written {
		t.Fatalf("written=%v err=%v", written, err)
	}
	cp, err := s.Load("cut")
	if err != nil {
		t.Fatal(err)
	}
	if cp.Key != key || string(cp.Payload) != `{"a":1}` {
		t.Fatalf("bad checkpoint: %+v", cp)
	}
}

func TestCommitIdempotent(t *testing.T) {
	s, _ := Open(t.TempDir())
	key := Key{Node: "n", InputDigest: "i", ParamsDigest: "p", Version: "v"}.IdempotencyKey()
	s.Commit("n", key, []byte(`1`))
	written, err := s.Commit("n", key, []byte(`1`))
	if err != nil {
		t.Fatal(err)
	}
	if written {
		t.Fatal("re-commit with same key must be a no-op")
	}
	// Different params -> new key -> new commit.
	key2 := Key{Node: "n", InputDigest: "i", ParamsDigest: "p2", Version: "v"}.IdempotencyKey()
	written, _ = s.Commit("n", key2, []byte(`2`))
	if !written {
		t.Fatal("param change must produce a new commit")
	}
}

func TestLoadMissing(t *testing.T) {
	s, _ := Open(t.TempDir())
	cp, err := s.Load("ghost")
	if err != nil || cp != nil {
		t.Fatalf("missing checkpoint should be (nil,nil), got %v %v", cp, err)
	}
}

func TestLoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	s.Commit("cut", "k", []byte(`{"a":1}`))
	// Flip bytes inside the committed file.
	p := filepath.Join(dir, "cut.json")
	data, _ := os.ReadFile(p)
	for i := range data {
		if data[i] == '1' {
			data[i] = '2'
			break
		}
	}
	os.WriteFile(p, data, 0o644)
	_, err := s.Load("cut")
	if clock.CodeOf(err) != clock.CodeCheckpointCorrupt {
		t.Fatalf("want CHECKPOINT_CORRUPT, got %v", err)
	}
}

func TestNodeNameValidated(t *testing.T) {
	s, _ := Open(t.TempDir())
	if _, err := s.Commit("../evil", "k", []byte(`1`)); clock.CodeOf(err) != clock.CodePathEscape {
		t.Fatalf("want PATH_ESCAPE, got %v", err)
	}
}
