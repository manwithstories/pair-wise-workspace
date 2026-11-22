package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFingerprintStableAndSensitive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.bin")
	if err := os.WriteFile(p, []byte("media-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	h1, err := FingerprintFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(h1) != 64 {
		t.Fatalf("fingerprint length = %d, want 64 hex chars", len(h1))
	}
	h2, _ := FingerprintFile(p)
	if h1 != h2 {
		t.Fatal("same input must hash identically")
	}
	if err := os.WriteFile(p, []byte("media-bytesX"), 0o600); err != nil {
		t.Fatal(err)
	}
	h3, _ := FingerprintFile(p)
	if h3 == h1 {
		t.Fatal("changed input must change fingerprint")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Fingerprints: map[string]string{"x": "abc"}, Params: "p1",
		CursorStage: StageVerify, CursorIndex: 1}
	if err := s.AppendSegment(dir, Segment{ID: "s1", Source: "x", Start: 0, End: 10, Stage: StageVerify}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Params != "p1" || loaded.CursorIndex != 1 || len(loaded.Segments) != 1 {
		t.Fatalf("round trip lost data: %+v", loaded)
	}
	// No temp file may remain after the atomic rename.
	if _, err := os.Stat(filepath.Join(dir, ".checkpoint.tmp")); !os.IsNotExist(err) {
		t.Fatal("atomic write left temp file behind")
	}
}

func TestLoadMissingReturnsNil(t *testing.T) {
	s, err := Load(t.TempDir())
	if err != nil || s != nil {
		t.Fatalf("missing checkpoint must be (nil,nil), got %v %v", s, err)
	}
}

func TestRejectChangedInput(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(p, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	fp, _ := FingerprintFile(p)
	s := &Store{Fingerprints: map[string]string{p: fp}, Params: "p"}
	if err := s.VerifyFingerprint(); err != nil {
		t.Fatalf("unchanged input must verify: %v", err)
	}
	if err := os.WriteFile(p, []byte("modified"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyFingerprint(); err == nil {
		t.Fatal("changed input must be refused")
	}
	// Missing input is also a hard refusal.
	s2 := &Store{Fingerprints: map[string]string{filepath.Join(dir, "gone"): fp}}
	if err := s2.VerifyFingerprint(); err == nil {
		t.Fatal("missing input must be refused")
	}
}

func TestRejectDifferentParams(t *testing.T) {
	s := &Store{Params: "rate=25"}
	if err := s.VerifyParams("rate=25"); err != nil {
		t.Fatal("same params must pass")
	}
	if err := s.VerifyParams("rate=30"); err == nil {
		t.Fatal("different params must be refused")
	}
}

func TestCursorOnlyMovesForward(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Params: "p"}
	if err := s.AppendSegment(dir, Segment{ID: "a", Start: 0, End: 100, Stage: StageStream}); err != nil {
		t.Fatal(err)
	}
	// Adjacent segment is fine.
	if err := s.AppendSegment(dir, Segment{ID: "b", Start: 100, End: 200, Stage: StageStream}); err != nil {
		t.Fatalf("adjacent segment rejected: %v", err)
	}
	// Overlapping/backwards cursor must be refused.
	if err := s.AppendSegment(dir, Segment{ID: "c", Start: 150, End: 180, Stage: StageStream}); err == nil {
		t.Fatal("backwards cursor must be refused")
	}
	if s.CursorIndex != 2 {
		t.Fatalf("failed append must not move cursor: %d", s.CursorIndex)
	}
}

func TestCorruptCheckpointRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("corrupt checkpoint must return error, not be silently ignored")
	}
}
