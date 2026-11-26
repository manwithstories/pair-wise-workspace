package cache

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/archive/tsrestore/internal/timeline"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMissPutGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	k := Key{Rendition: "v1", SegmentID: "v1-000003", SourceHash: digest([]byte("src-3"))}
	if _, hit, err := s.Get(k); err != nil || hit {
		t.Fatalf("fresh store should miss, got hit=%v err=%v", hit, err)
	}
	payload := []byte("segment-bytes")
	out, err := s.Put(k, payload)
	if err != nil {
		t.Fatal(err)
	}
	if out != digest(payload) {
		t.Fatal("Put returned wrong output hash")
	}
	got, hit, err := s.Get(k)
	if err != nil || !hit || string(got) != string(payload) {
		t.Fatalf("cache hit failed: hit=%v err=%v len=%d", hit, err, len(got))
	}
}

func TestDifferentSourceHashIsDistinctEntry(t *testing.T) {
	s := newTestStore(t)
	k1 := Key{Rendition: "v1", SegmentID: "v1-000001", SourceHash: digest([]byte("a"))}
	k2 := Key{Rendition: "v1", SegmentID: "v1-000001", SourceHash: digest([]byte("b"))}
	if _, err := s.Put(k1, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if _, hit, _ := s.Get(k2); hit {
		t.Fatal("different source hash must not collide on the same entry")
	}
}

func TestTamperedOutputHashRejected(t *testing.T) {
	s := newTestStore(t)
	k := Key{Rendition: "v2", SegmentID: "v2-000007", SourceHash: digest([]byte("src"))}
	if _, err := s.Put(k, []byte("original")); err != nil {
		t.Fatal(err)
	}
	// Corrupt the payload bytes on disk, leaving the metadata claiming the
	// original hash: check 3 must fail and the entry is invalidated.
	payloadPath, _ := s.paths(k)
	if err := os.WriteFile(payloadPath, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, hit, err := s.Get(k)
	if !errors.Is(err, timeline.ErrIntegrity) || hit {
		t.Fatalf("expected integrity rejection, got hit=%v err=%v", hit, err)
	}
	// After invalidation the entry is gone: a subsequent Get is a clean miss.
	if _, hit, err := s.Get(k); err != nil || hit {
		t.Fatalf("invalidated entry should miss, got hit=%v err=%v", hit, err)
	}
}

func TestTamperedSourceDigestRejected(t *testing.T) {
	s := newTestStore(t)
	k := Key{Rendition: "v1", SegmentID: "v1-000002", SourceHash: digest([]byte("real"))}
	if _, err := s.Put(k, []byte("data")); err != nil {
		t.Fatal(err)
	}
	_, metaPath := s.paths(k)
	raw, _ := os.ReadFile(metaPath)
	var e Entry
	if err := jsonUnmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	e.SourceHash = digest([]byte("forged"))
	writeJSON(t, metaPath, e)
	if _, hit, err := s.Get(k); !errors.Is(err, timeline.ErrIntegrity) || hit {
		t.Fatalf("source digest tamper must be rejected: hit=%v err=%v", hit, err)
	}
}

func TestTamperedIdentityRejected(t *testing.T) {
	s := newTestStore(t)
	k := Key{Rendition: "v1", SegmentID: "v1-000005", SourceHash: digest([]byte("src"))}
	if _, err := s.Put(k, []byte("data")); err != nil {
		t.Fatal(err)
	}
	_, metaPath := s.paths(k)
	raw, _ := os.ReadFile(metaPath)
	var e Entry
	if err := jsonUnmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	e.Rendition = "attacker"
	writeJSON(t, metaPath, e)
	if _, hit, err := s.Get(k); !errors.Is(err, timeline.ErrIntegrity) || hit {
		t.Fatalf("identity tamper must be rejected: hit=%v err=%v", hit, err)
	}
}
