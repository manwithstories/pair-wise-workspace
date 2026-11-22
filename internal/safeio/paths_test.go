package safeio

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveStaysInside(t *testing.T) {
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(root, "sub/clip.mov")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(resolvedRoot, "sub", "clip.mov"); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if _, err := Resolve(root, "a/../../etc/passwd"); !errors.Is(err, ErrEscape) {
		t.Fatalf("want escape, got %v", err)
	}
	if _, err := Resolve(root, "/etc/passwd"); !errors.Is(err, ErrAbsolute) {
		t.Fatalf("want absolute rejection, got %v", err)
	}
}

func TestResolveRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ")
	}
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.bin")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "leak")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(root, "leak/secret.bin"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("want symlink escape, got %v", err)
	}
}

func TestResolveAllowsInternalSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ")
	}
	root := t.TempDir()
	inner := filepath.Join(root, "real")
	if err := os.Mkdir(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inner, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(root, "alias/file.mov"); err != nil {
		t.Fatalf("internal symlink should pass: %v", err)
	}
}
