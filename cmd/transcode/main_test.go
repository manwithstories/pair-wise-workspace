package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckPathRejectsTraversalAndSymlink(t *testing.T) {
	if err := checkPath("../secret"); err == nil {
		t.Fatal("parent traversal was accepted")
	}
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := checkPath(link); err == nil {
		t.Fatal("symlink was accepted")
	}
	if err := checkPath(filepath.Join(dir, "new", "work")); err != nil {
		t.Fatalf("nonexistent safe path rejected: %v", err)
	}
}
