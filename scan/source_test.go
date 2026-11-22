package scan

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadManifestRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	evil := filepath.Join(dir, "evil.json")
	body := `{"version":"mediaslice-source/v1","segment_ticks":180000,` +
		`"sources":[{"id":"a","bitrate":1,"mp4":"../../etc/passwd"}]}`
	if err := os.WriteFile(evil, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(evil); err == nil {
		t.Fatal("traversal manifest accepted")
	}

	abs := filepath.Join(dir, "abs.json")
	body2 := `{"version":"mediaslice-source/v1","segment_ticks":180000,` +
		`"sources":[{"id":"a","bitrate":1,"mp4":"` +
		filepath.Join(dir, "x.mp4") + `"}]}`
	if err := os.WriteFile(abs, []byte(body2), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(abs); err == nil {
		t.Fatal("absolute path manifest accepted")
	}
}

func TestLoadManifestValidAndResolves(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.mp4"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mPath := filepath.Join(dir, "src.json")
	body := `{"version":"mediaslice-source/v1","segment_ticks":180000,` +
		`"sources":[{"id":"a","bitrate":800000,"mp4":"a.mp4","rtp":"a.rtp"}]}`
	if err := os.WriteFile(mPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(mPath)
	if err != nil {
		t.Fatal(err)
	}
	if m.Sources[0].MP4Path != filepath.Join(dir, "a.mp4") {
		t.Fatalf("mp4 not resolved absolute: %s", m.Sources[0].MP4Path)
	}
	if m.Version != ManifestVersion {
		t.Fatal("version mismatch")
	}
}

func TestLoadManifestRejectsBadVersion(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "old.json")
	if err := os.WriteFile(p, []byte(`{"version":"old","sources":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(p); err == nil {
		t.Fatal("unsupported version accepted")
	}
}
