package fsguard

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveRejectsEscape(t *testing.T) {
	root := t.TempDir()
	r, err := OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("../outside.mkv"); !errors.Is(err, ErrEscape) {
		t.Fatalf("lexical escape not rejected: %v", err)
	}
	if _, err := r.Resolve("/etc/passwd"); !errors.Is(err, ErrEscape) {
		t.Fatalf("absolute escape not rejected: %v", err)
	}
	if _, err := r.Resolve("sub/file.mkv"); err != nil {
		t.Fatalf("root-relative path rejected: %v", err)
	}
}

func TestResolveRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.mkv")
	if err := os.WriteFile(secret, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.mkv")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r, err := OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("link.mkv"); !errors.Is(err, ErrEscape) {
		t.Fatalf("symlink escape not rejected: %v", err)
	}
}

func TestResolveAllowsInternalSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real.mkv")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "alias.mkv")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r, err := OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	r.FollowSymlinks = true
	got, err := r.Resolve("alias.mkv")
	if err != nil {
		t.Fatalf("internal symlink rejected: %v", err)
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if got != realTarget {
		t.Fatalf("got %s want %s", got, realTarget)
	}
}

func TestAtomicFileAbortLeavesNoTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.mp4")
	a, err := CreateOutput(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	tmp := a.Name()
	if err := a.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target exists after abort: %v", err)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file survived abort: %v", err)
	}
}

func TestAtomicFileCommit(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.mp4")
	a, err := CreateOutput(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write([]byte("complete")); err != nil {
		t.Fatal(err)
	}
	if err := a.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "complete" {
		t.Fatalf("commit failed: %q %v", got, err)
	}
	if err := a.Abort(); err != nil {
		t.Fatalf("abort after commit must be a no-op: %v", err)
	}
}

func TestGuardOutputRejectsInputOverwrite(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "master.mkv"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.GuardOutput("master.mkv", "master.mkv"); !errors.Is(err, ErrOutputInsideInput) {
		t.Fatalf("got %v", err)
	}
	if _, err := r.GuardOutput("master.mkv", "./master.mkv"); !errors.Is(err, ErrOutputInsideInput) {
		t.Fatalf("got %v for equivalent path", err)
	}
	out, err := r.GuardOutput("master.mkv", "out/master.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(out) {
		t.Fatalf("output not absolute: %s", out)
	}
}

func TestGuardOutputRejectsEscape(t *testing.T) {
	r, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.GuardOutput("in.mkv", "../out.mp4"); !errors.Is(err, ErrEscape) {
		t.Fatalf("got %v", err)
	}
}
