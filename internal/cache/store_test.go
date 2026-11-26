package cache

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediapipe/internal/media"
)

func writeSource(t *testing.T, content string) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "source.mp4")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := media.FileSHA256(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, sum
}

func TestKeyBindsSourceAndConfig(t *testing.T) {
	p, sum := writeSource(t, "asset-one")
	defer os.Remove(p)
	k1, err := Key(sum, map[string]any{"bitrate_kbps": 1200})
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := Key(sum, map[string]any{"bitrate_kbps": 1500})
	k3, _ := Key(sum+"x", map[string]any{"bitrate_kbps": 1200})
	if k1 == k2 || k1 == k3 {
		t.Fatal("cache key must change when source or config changes")
	}
}

func TestCacheHitStillVerifiesSourceAndArtifactSHA(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	s, err := Open(root, 16, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	src, sum := writeSource(t, "broadcast-asset")
	key, _ := Key(sum, map[string]string{"profile": "air"})
	if _, err := s.Put(key, sum, "cfgsha", "seg000.mp4", []byte("MP4DATA")); err != nil {
		t.Fatal(err)
	}
	v, err := s.VerifyHit(key, src)
	if err != nil || !v.Hit || !v.SourceOK || !v.SHA256OK {
		t.Fatalf("expected verified hit: %+v err=%v", v, err)
	}

	art, _ := s.ArtifactPath(key, "seg000.mp4")
	if err := os.WriteFile(art, []byte("TAMPERED"), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err = s.VerifyHit(key, src)
	if err != nil {
		t.Fatal(err)
	}
	if v.Hit || v.SHA256OK {
		t.Fatalf("tampered artifact must invalidate: %+v", v)
	}
	var me *media.Error
	err = StrictVerification(v, nil)
	if !errors.As(err, &me) || me.ExitCode() != 3 || me.Code != media.CodeChecksumMismatch {
		t.Fatalf("expected checksum exit 3, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "entries", key+".json")); !os.IsNotExist(err) {
		t.Fatal("tampered entry was not removed")
	}
}

func TestChangedSourceInvalidatesCache(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "cache"), 16, 1<<20)
	src, sum := writeSource(t, "original")
	key, _ := Key(sum, nil)
	_, _ = s.Put(key, sum, "", "a.mp4", []byte("a"))
	if err := os.WriteFile(src, []byte("changed-source"), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := s.VerifyHit(key, src)
	if err != nil {
		t.Fatal(err)
	}
	if v.Hit || v.SourceOK {
		t.Fatalf("source change must miss: %+v", v)
	}
	err = StrictVerification(v, nil)
	var me *media.Error
	if !errors.As(err, &me) || me.Code != media.CodeSourceMismatch || me.ExitCode() != 3 {
		t.Fatalf("expected source mismatch exit3, got %v", err)
	}
}

func TestCapacityIsBounded(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "cache"), 1, 1<<20)
	_, sum := writeSource(t, "capacity")
	for i := 0; i < 3; i++ {
		key, _ := Key(sum, i)
		if _, err := s.Put(key, sum, "", "a.mp4", []byte("data")); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.listLocked()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 1 {
		t.Fatalf("cache exceeded entry capacity: %d", len(entries))
	}
}

func TestRejectNonHexKey(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "cache"), 1, 1<<20)
	if _, err := s.entryPath("../../etc"); err == nil {
		t.Fatal("expected traversal/non-hex key rejection")
	}
}
