package safepath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJoinInside(t *testing.T) {
	dir := t.TempDir()
	p, err := Join(dir, "out/frames/frame.pgm")
	if err != nil {
		t.Fatal(err)
	}
	evalDir, _ := filepath.EvalSymlinks(dir) // macOS temp dirs are symlinked
	want := filepath.Join(evalDir, "out", "frames", "frame.pgm")
	if p != want {
		t.Fatalf("unexpected path %q, want %q", p, want)
	}
}

func TestJoinEscape(t *testing.T) {
	dir := t.TempDir()
	if _, err := Join(dir, "../etc/passwd"); err == nil {
		t.Fatal("path escape accepted")
	}
}

func TestJoinSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlink unavailable")
	}
	if _, err := Join(dir, "link/secret"); err == nil {
		t.Fatal("symlink escape accepted")
	}
	// A symlink pointing inside the work dir is fine.
	inner := filepath.Join(dir, "inner")
	os.Mkdir(inner, 0o755)
	os.Symlink(inner, filepath.Join(dir, "ok"))
	if _, err := Join(dir, "ok/file"); err != nil {
		t.Fatalf("inner symlink rejected: %v", err)
	}
}
