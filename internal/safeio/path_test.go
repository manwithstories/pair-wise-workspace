package safeio

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveInsideRoot(t *testing.T) {
	root := t.TempDir()
	got, err := Resolve(root, filepath.Join("cam", "a.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(got) != filepath.Join(root, "cam") {
		t.Fatalf("resolved = %s", got)
	}
}

func TestResolveRejectsEscape(t *testing.T) {
	root := t.TempDir()
	if _, err := Resolve(root, filepath.Join("..", "outside.mp4")); err == nil {
		t.Fatal("parent traversal must be rejected")
	}
	abs := filepath.Join(root, "sub")
	_ = os.MkdirAll(abs, 0o755)
	if _, err := Resolve(root, abs+"/../../etc/passwd"); err == nil {
		t.Fatal("normalized escape must be rejected")
	}
}

func TestResolveRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.mp4")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.mp4")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(root, "link.mp4"); err == nil {
		t.Fatal("symlink path must be rejected")
	}
}

func TestOpenInput(t *testing.T) {
	root := t.TempDir()
	rel := filepath.Join("in", "f.mp4")
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, got, err := OpenInput(root, rel)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got != abs {
		t.Fatalf("path = %s", got)
	}
}
