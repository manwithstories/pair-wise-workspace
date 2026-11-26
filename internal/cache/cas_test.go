package cache

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pairwise/relay/internal/media"
)

func testKey(source string) Key {
	return Key{SourceNode: source, StreamID: "event-17", Rendition: "high", Segment: 9}
}

func TestCachePutGetVerifiesProvenanceAndHash(t *testing.T) {
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("deterministic fmp4 fragment payload")
	meta, err := c.Put(testKey("edge-a"), data)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.SHA256) != 64 {
		t.Fatalf("sha length=%d", len(meta.SHA256))
	}
	got, gotMeta, err := c.Get(testKey("edge-a"))
	if err != nil {
		t.Fatalf("cache hit: %v", err)
	}
	if string(got) != string(data) || gotMeta.SHA256 != meta.SHA256 {
		t.Fatal("returned cache entry differs")
	}
	if c.RehashCount() < 2 { // Put hash + Get rehash
		t.Fatalf("rehash count=%d", c.RehashCount())
	}
}

func TestCacheOriginMismatchQuarantinesAndReprocesses(t *testing.T) {
	c, _ := Open(t.TempDir())
	if _, err := c.Put(testKey("edge-forged"), []byte("forged provenance")); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.Get(testKey("edge-a"))
	if err == nil {
		t.Fatal("forged origin cache hit accepted")
	}
	f, ok := media.AsFault(err)
	if !ok || f.Code != media.CodeCacheOrigin {
		t.Fatalf("err=%v want cache_origin_mismatch", err)
	}
	matches, _ := filepath.Glob(filepath.Join(c.Root(), "quarantine", "*"))
	if len(matches) == 0 {
		t.Fatal("forged entry was not quarantined")
	}
	if _, _, err := c.Get(testKey("edge-a")); err == nil {
		t.Fatal("quarantined reference still produces a hit")
	}
	if _, err := c.Put(testKey("edge-a"), []byte("reprocessed fragment")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Get(testKey("edge-a")); err != nil {
		t.Fatalf("fresh entry should hit: %v", err)
	}
}

func TestCacheDigestMismatchQuarantines(t *testing.T) {
	c, _ := Open(t.TempDir())
	key := testKey("edge-a")
	meta, err := c.Put(key, []byte("original bytes"))
	if err != nil {
		t.Fatal(err)
	}
	obj := c.objectPath(meta.SHA256)
	if err := os.WriteFile(obj, []byte("corrupt bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	_, _, err = c.Get(key)
	f, ok := media.AsFault(err)
	if !ok || f.Code != media.CodeCacheDigest {
		t.Fatalf("err=%v want cache_digest_mismatch", err)
	}
	if _, err := os.Stat(obj); !os.IsNotExist(err) {
		t.Fatalf("corrupt object should be removed/quarantined, stat err=%v", err)
	}
}

func TestCacheRejectsUnsafeProvenancePath(t *testing.T) {
	c, _ := Open(t.TempDir())
	key := testKey("edge-a")
	key.StreamID = "../escape"
	if _, err := c.Put(key, []byte("x")); err == nil {
		t.Fatal("path traversal key accepted")
	}
}
