package cache

import (
	"os"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAddressIsContentAddressed(t *testing.T) {
	if Key([]byte("a")) != Key([]byte("a")) {
		t.Fatal("address is not stable")
	}
	if Key([]byte("a")) == Key([]byte("b")) {
		t.Fatal("different content shares an address")
	}
	if len(Key([]byte("a"))) != 64 {
		t.Fatal("address is not a full sha256")
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	s := newStore(t)
	data := []byte("material payload")
	src := Provenance("deadbeef", 1, 324000000, 1116, 12927)
	e, err := s.Put(data, src)
	if err != nil {
		t.Fatal(err)
	}
	if e.Address != Key(data) || e.Length != int64(len(data)) {
		t.Fatalf("entry = %+v", e)
	}
	got, hit, err := s.Get(e.Address, src)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != string(data) {
		t.Fatal("payload altered")
	}
	if hit.Source.String() != src.String() {
		t.Fatalf("provenance = %q", hit.Source)
	}
	if s.Hits != 1 {
		t.Fatalf("hits = %d", s.Hits)
	}
}

func TestHitWithWrongProvenanceIsRejected(t *testing.T) {
	s := newStore(t)
	data := []byte("payload")
	good := Provenance("aaa", 1, 10, 0, 100)
	if _, err := s.Put(data, good); err != nil {
		t.Fatal(err)
	}
	other := Provenance("bbb", 1, 10, 0, 100) // same bytes, different source
	_, _, err := s.Get(Key(data), other)
	if err == nil {
		t.Fatal("expected provenance mismatch")
	}
	if s.Quarantined != 1 || len(s.Rejected) != 1 {
		t.Fatalf("quarantined = %d rejected = %v", s.Quarantined, s.Rejected)
	}
}

func TestHitWithWrongLengthIsRejected(t *testing.T) {
	s := newStore(t)
	data := []byte("payload-1234")
	src := Provenance("x", 2, 0, 0, 9)
	e, err := s.Put(data, src)
	if err != nil {
		t.Fatal(err)
	}
	// Tamper the metadata to declare a different length.
	e.Length = 3
	_, verr := s.Verify(e, src)
	if verr == nil {
		t.Fatal("expected length mismatch")
	}
	bad, ok := verr.(*Mismatch)
	if !ok || bad.Reason != "length mismatch" {
		t.Fatalf("mismatch = %#v", verr)
	}
	if s.Quarantined != 1 {
		t.Fatalf("quarantined = %d", s.Quarantined)
	}
}

func TestCorruptObjectIsQuarantinedAndReprocessed(t *testing.T) {
	s := newStore(t)
	data := []byte("original bytes")
	src := Provenance("m", 1, 7, 0, 14)
	e, err := s.Put(data, src)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the cached object on disk, preserving its length.
	path := s.objectPath(e.Address)
	if err := os.WriteFile(path, []byte("tampered bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Get(e.Address, src)
	if err == nil {
		t.Fatal("expected digest mismatch")
	}
	m, ok := err.(*Mismatch)
	if !ok || m.Reason != "digest mismatch" {
		t.Fatalf("err = %#v", err)
	}
	if s.Quarantined != 1 {
		t.Fatalf("quarantined = %d", s.Quarantined)
	}
	// Re-processing the correct bytes heals the cache.
	if _, err := s.Put(data, src); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Get(e.Address, src)
	if err != nil {
		t.Fatalf("healed get: %v", err)
	}
	if string(got) != string(data) {
		t.Fatal("healed payload")
	}
	// The quarantine file remains for diagnosis.
	bad, err := s.QuarantinedEntries()
	if err != nil || len(bad) == 0 {
		t.Fatalf("quarantine files = %v err=%v", bad, err)
	}
}

func TestMetadataDigestMismatch(t *testing.T) {
	s := newStore(t)
	data := []byte("bytes")
	src := Provenance("m", 3, 0, 0, 5)
	e, err := s.Put(data, src)
	if err != nil {
		t.Fatal(err)
	}
	e.Digest = Key([]byte("other"))
	if _, err := s.Verify(e, src); err == nil {
		t.Fatal("expected digest metadata mismatch")
	}
}

func TestMissingEntryIsMiss(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.Get(Key([]byte("absent")), Provenance("m", 1, 0, 0, 1)); err == nil {
		t.Fatal("expected a miss")
	}
	if s.Misses != 1 {
		t.Fatalf("misses = %d", s.Misses)
	}
}

func TestEntriesAreOwnerOnly(t *testing.T) {
	s := newStore(t)
	if _, err := s.Put([]byte("x"), Provenance("m", 1, 0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.objectPath(Key([]byte("x"))))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("object mode = %v", info.Mode().Perm())
	}
	if info, err := os.Stat(s.Root); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("root mode = %v err=%v", info.Mode().Perm(), err)
	}
}

func TestFileDigestMatchesKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.bin")
	data := []byte("0123456789")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := FileDigest(p)
	if err != nil || d != Key(data) {
		t.Fatalf("digest = %q want %q err=%v", d, Key(data), err)
	}
}

func TestVerifyObjectStandalone(t *testing.T) {
	data := []byte("abc")
	up := Entry{Address: Key(data), Length: 3, Digest: Key(data), Source: Provenance("m", 1, 0, 0, 3)}
	if err := VerifyObject(data, up, up.Source); err != nil {
		t.Fatal(err)
	}
	if err := VerifyObject(data, up, Provenance("n", 1, 0, 0, 3)); err == nil {
		t.Fatal("wrong provenance accepted")
	}
}
