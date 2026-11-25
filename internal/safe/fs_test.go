package safe_test

import (
	"os"
	"path/filepath"
	"testing"

	"bcast/internal/safe"
)

func TestResolveRejectsTraversalAndSymlink(t *testing.T) {
	dir := t.TempDir()
	root, err := safe.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.Resolve("../escape.mp4"); err == nil {
		t.Fatal("path traversal must be rejected")
	}

	outside := filepath.Join(t.TempDir(), "outside.mp4")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.mp4")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := root.Resolve("link.mp4"); err == nil {
		t.Fatal("symlink inputs must be rejected")
	}
}

func TestCommitTempVerifiesDigest(t *testing.T) {
	dir := t.TempDir()
	root, err := safe.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("out/final.tmp", []byte("segment-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := root.CommitTemp("out/final.tmp", "out/final.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(sum) != 64 {
		t.Fatalf("digest length=%d", len(sum))
	}
	if err := root.VerifyHash("out/final.bin", sum); err != nil {
		t.Fatal(err)
	}
	if err := root.VerifyHash("out/final.bin", "bad"); err == nil {
		t.Fatal("bad digest must fail verification")
	}
}
