package safepath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveRejectsTraversal(t *testing.T) {
	for _, p := range []string{"", "   ", "../x", "a/../../b", "/tmp/../etc"} {
		if _, err := Resolve(p); err == nil {
			t.Errorf("Resolve(%q) unexpectedly succeeded", p)
		}
	}
	abs, err := Resolve("out/frames")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(abs) {
		t.Fatalf("not absolute: %s", abs)
	}
}

func TestWriteAtomicAndJoin(t *testing.T) {
	root := t.TempDir()
	target, err := Join(root, "sub/file.ppm")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(target, []byte("data"), root); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "data" {
		t.Fatalf("read back = %q, %v", got, err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 1 {
		t.Fatalf("stray temp files: %v", entries)
	}
}

func TestWriteAtomicRefusesEscape(t *testing.T) {
	root := t.TempDir()
	if err := WriteAtomic(filepath.Join(root, "..", "escape.txt"), []byte("x"), root); err == nil {
		t.Fatal("expected path escape rejection")
	}
	if _, err := Join(root, "../escape"); err == nil {
		t.Fatal("Join allowed traversal")
	}
	if _, err := Join(root, "/abs"); err == nil {
		t.Fatal("Join allowed absolute path")
	}
}
