package cache_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fashionweek/mediad/internal/cache"
	"github.com/fashionweek/mediad/internal/media"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPutGetVerifyAndRestore(t *testing.T) {
	dir := t.TempDir()
	store, err := cache.Open(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "in.mp4")
	writeFile(t, src, "camera-bytes")
	inputHash, err := cache.HashFile(src)
	if err != nil {
		t.Fatal(err)
	}
	config := []byte(`{"segment_seconds":2}`)
	tool := "ffmpeg 9.1"
	key := cache.Key(inputHash, config, tool)

	out := filepath.Join(dir, "seg0.m4s")
	writeFile(t, out, "segment-bytes")
	if _, err := store.Put(key, inputHash, tool, config, map[string]string{"seg0": out}); err != nil {
		t.Fatal(err)
	}
	// Hit: re-verify with the unchanged input must succeed.
	got, err := store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected cache hit")
	}
	if err := store.Verify(got, inputHash); err != nil {
		t.Fatalf("fresh entry should verify: %v", err)
	}
	// Restore the blob into a new location and confirm identical content.
	dest := filepath.Join(dir, "restored.m4s")
	if err := store.Restore(got, "seg0", dest); err != nil {
		t.Fatal(err)
	}
	if cache.HashBytes(readFile(t, dest)) != cache.HashBytes([]byte("segment-bytes")) {
		t.Fatal("restored blob content mismatch")
	}
}

func TestSourceChangeInvalidatesEntry(t *testing.T) {
	dir := t.TempDir()
	store, _ := cache.Open(filepath.Join(dir, "cas"))
	src := filepath.Join(dir, "in.mp4")
	writeFile(t, src, "v1")
	h1, _ := cache.HashFile(src)
	config := []byte("cfg")
	key := cache.Key(h1, config, "tool")
	out := filepath.Join(dir, "o.m4s")
	writeFile(t, out, "out")
	entry, err := store.Put(key, h1, "tool", config, map[string]string{"o": out})
	if err != nil {
		t.Fatal(err)
	}
	// Source changed: new input hash must fail verification.
	if err := store.Verify(entry, cache.HashBytes([]byte("different-source"))); err == nil {
		t.Fatal("changed source must invalidate the cache entry")
	} else if !media.IsKind(err, media.KindDigest) {
		t.Fatalf("want digest mismatch, got %v", err)
	}
}

func TestKeyChangesWithConfigAndTool(t *testing.T) {
	input := cache.HashBytes([]byte("input"))
	k1 := cache.Key(input, []byte("cfg-a"), "ffmpeg 9.1")
	k2 := cache.Key(input, []byte("cfg-b"), "ffmpeg 9.1")
	k3 := cache.Key(input, []byte("cfg-a"), "ffmpeg 10.0")
	if k1 == k2 || k1 == k3 {
		t.Fatal("cache key must differ when config or tool version changes")
	}
}

func TestMissingGetReturnsNil(t *testing.T) {
	store, _ := cache.Open(filepath.Join(t.TempDir(), "cas"))
	got, err := store.Get("nonexistent-key-1234")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("missing key should return nil entry")
	}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
