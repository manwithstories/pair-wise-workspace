package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPutGetVerified(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("segment-bytes")
	obj, existed, err := s.Put("media/seg-1.m4s", data)
	if err != nil {
		t.Fatal(err)
	}
	if existed {
		t.Fatal("first Put must not report a hit")
	}
	if obj.Digest != Digest(data) || obj.Source != "media/seg-1.m4s" {
		t.Fatalf("bad object: %+v", obj)
	}
	got, err := s.Get(obj.Digest, "media/seg-1.m4s")
	if err != nil || string(got) != string(data) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if !s.Verified(obj.Digest, "media/seg-1.m4s") {
		t.Fatal("fresh entry must verify")
	}
	again, existed, err := s.Put("media/seg-1.m4s", data)
	if err != nil || !existed || again.Digest != obj.Digest {
		t.Fatalf("second Put should be a verified hit: %+v %v %v", again, existed, err)
	}
}

func TestSourceValidation(t *testing.T) {
	bad := []string{"", "/abs/seg.m4s", "http://x/seg.m4s", "../seg.m4s", "a/../../b", `a\b`}
	for _, src := range bad {
		if err := ValidateSource(src); err == nil {
			t.Errorf("%q: expected rejection", src)
		}
	}
	if err := ValidateSource("show/s01/seg-00001.m4s"); err != nil {
		t.Errorf("relative provenance rejected: %v", err)
	}
}

func TestGetProvenanceMismatch(t *testing.T) {
	s, _ := Open(t.TempDir())
	obj, _, err := s.Put("a/seg.m4s", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(obj.Digest, "b/seg.m4s"); err == nil {
		t.Fatal("requesting a different source must fail")
	}
	if s.Verified(obj.Digest, "b/seg.m4s") {
		t.Fatal("Verified must reject provenance mismatch")
	}
}

func TestCorruptBlobQuarantinedAndRecomputed(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	data := []byte("original")
	obj, _, err := s.Put("m/seg.m4s", data)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the blob behind the store's back.
	if err := os.WriteFile(filepath.Join(root, "blobs", obj.Digest), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s.Verified(obj.Digest, "m/seg.m4s") {
		t.Fatal("corrupt blob must not verify")
	}
	_, existed, err := s.Put("m/seg.m4s", data)
	if err != nil {
		t.Fatal(err)
	}
	if existed {
		t.Fatal("corrupt hit must be recomputed, not served")
	}
	got, err := s.Get(obj.Digest, "m/seg.m4s")
	if err != nil || string(got) != "original" {
		t.Fatalf("recomputed Get = %q, %v", got, err)
	}
	// The bad blob must have been moved aside, not silently dropped.
	des, _ := os.ReadDir(filepath.Join(root, "quarantine"))
	if len(des) == 0 {
		t.Fatal("expected quarantined artifacts")
	}
}

func TestMetaMismatchIsolates(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	obj, _, err := s.Put("m/seg.m4s", []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite metadata to claim a different provenance.
	mp := filepath.Join(root, "meta", obj.Digest+".json")
	if err := os.WriteFile(mp, []byte(`{"digest":"`+obj.Digest+`","source":"evil/seg.m4s","bytes":7}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(obj.Digest, "m/seg.m4s"); err == nil {
		t.Fatal("metadata provenance mismatch must fail the read")
	}
}

func TestRejectOversize(t *testing.T) {
	s, _ := Open(t.TempDir())
	if _, _, err := s.Put("m/big.m4s", make([]byte, 64<<20+1)); err == nil {
		t.Fatal("segment over 64 MiB must be rejected")
	}
}

func TestQuarantineKeepsBothArtifacts(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	obj, _, err := s.Put("m/seg.m4s", []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.quarantine(obj.Digest); err != nil {
		t.Fatal(err)
	}
	des, err := os.ReadDir(filepath.Join(root, "quarantine"))
	if err != nil {
		t.Fatal(err)
	}
	var blob, meta int
	for _, de := range des {
		// Names are "<artifact>.<pid>.<n>", so look for the metadata
		// marker rather than the (now numeric) suffix.
		if strings.Contains(de.Name(), ".json.") {
			meta++
		} else {
			blob++
		}
	}
	if blob != 1 || meta != 1 {
		t.Fatalf("quarantine holds blob=%d meta=%d, want 1/1", blob, meta)
	}
	if _, err := os.Stat(filepath.Join(root, "blobs", obj.Digest)); !os.IsNotExist(err) {
		t.Fatal("blob must be gone from the live store")
	}
}

func TestRepeatedQuarantineDoesNotClobber(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	obj, _, err := s.Put("m/seg.m4s", []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	// First quarantine moves the artifacts; the second must simply find
	// them absent and still succeed without overwriting evidence.
	if err := s.quarantine(obj.Digest); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadDir(filepath.Join(root, "quarantine"))
	if err := s.quarantine(obj.Digest); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadDir(filepath.Join(root, "quarantine"))
	if len(first) != len(second) {
		t.Fatalf("quarantine entries changed on repeat: %d -> %d", len(first), len(second))
	}
}
