package cache

import (
	"os"
	"path/filepath"
	"testing"

	"avcheck/internal/fault"
)

func newChain(t *testing.T) *Chain {
	t.Helper()
	dir := t.TempDir()
	return &Chain{
		Dir:    filepath.Join(dir, "ck"),
		Store:  &Store{Dir: filepath.Join(dir, "cas")},
		Stages: []string{"parse", "recover", "mix", "render"},
	}
}

func TestCASHappyPathAndTamper(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	id, err := s.Put([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(id)
	if err != nil || string(got) != "hello" {
		t.Fatalf("get %v %q", err, got)
	}
	// Corrupt the blob on disk: Get must refuse it.
	p := filepath.Join(s.Dir, id)
	if err := os.WriteFile(p, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(id); !fault.IsCode(err, fault.Corrupt) {
		t.Fatalf("want corrupt, got %v", err)
	}
	if s.Has(id) {
		t.Fatal("tampered blob must not count as present")
	}
}

func TestCheckpointChainAndInvalidation(t *testing.T) {
	ch := newChain(t)
	blob1, _ := ch.Store.Put([]byte("stage1"))
	blob2, _ := ch.Store.Put([]byte("stage2"))
	c1 := &Checkpoint{Stage: "parse", SourceID: "src", SourceDigest: "sd",
		ParamDigest: "pd", PrevDigest: "-", BlobID: blob1}
	if err := ch.Save(c1); err != nil {
		t.Fatal(err)
	}
	c2 := &Checkpoint{Stage: "recover", SourceID: "src", SourceDigest: "sd",
		ParamDigest: "pd", PrevDigest: c1.Digest, BlobID: blob2}
	if err := ch.Save(c2); err != nil {
		t.Fatal(err)
	}
	// Wrong predecessor digest must be rejected.
	bad := &Checkpoint{Stage: "mix", SourceID: "src", SourceDigest: "sd",
		ParamDigest: "pd", PrevDigest: "bogus", BlobID: blob1}
	if err := ch.Save(bad); !fault.IsCode(err, fault.Corrupt) {
		t.Fatalf("want corrupt, got %v", err)
	}
	// Cache hit with matching identities.
	if _, ok := ch.Valid("recover", "src", "sd", "pd"); !ok {
		t.Fatal("expected valid checkpoint")
	}
	// Source digest mismatch invalidates this stage (and later ones).
	if _, ok := ch.Valid("parse", "src", "CHANGED", "pd"); ok {
		t.Fatal("mismatch must not validate")
	}
	if _, err := ch.Load("parse"); err == nil {
		t.Fatal("parse checkpoint should be invalidated")
	}
	if _, err := ch.Load("recover"); err == nil {
		t.Fatal("later stage should be invalidated too")
	}
}

func TestCheckpointTamperAndInterrupt(t *testing.T) {
	ch := newChain(t)
	blob, _ := ch.Store.Put([]byte("x"))
	c := &Checkpoint{Stage: "parse", SourceID: "s", SourceDigest: "d",
		ParamDigest: "p", PrevDigest: "-", BlobID: blob}
	if err := ch.Save(c); err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the checkpoint file: Load must detect it.
	p := ch.path("parse")
	data, _ := os.ReadFile(p)
	data[10] ^= 0x01
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ch.Load("parse"); !fault.IsCode(err, fault.Corrupt) {
		t.Fatalf("want corrupt, got %v", err)
	}
	// A leftover temp file from an interrupted save must not confuse Load.
	if err := os.WriteFile(filepath.Join(ch.Dir, ".tmp-ck-orphan"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ch.Load("parse"); !fault.IsCode(err, fault.Corrupt) {
		t.Fatalf("still want corrupt, got %v", err)
	}
}
