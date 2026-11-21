package safepath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJoinRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"../escape", "a/../../escape", "/etc/passwd", "", ".."} {
		if _, err := Join(root, name); err == nil {
			t.Fatalf("name %q accepted", name)
		}
	}
	got, err := Join(root, "nested/output.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(got) != filepath.Join(root, "nested") {
		t.Fatalf("got %s", got)
	}
}

func TestEnsureDir(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "work")
	if err := EnsureDir(target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err == nil {
		if err := EnsureDir(link); err == nil {
			t.Fatal("symlink accepted")
		}
	}
}

func TestEnsureFileRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real.mp4")
	if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.mp4")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink unavailable")
	}
	if err := EnsureFile(link); err == nil {
		t.Fatal("symlinked file accepted")
	}
}
