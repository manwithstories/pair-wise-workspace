package cache_test

import (
	"os"
	"path/filepath"
	"testing"

	"mediapack/internal/cache"
	"mediapack/internal/fault"
)

func TestPutGetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := cache.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := cache.Identity{SourceID: "stream://seed-17/h264", InputSHA256: "abc123"}
	blob := []byte("deterministic fragment bytes")
	key, err := st.Put(id, blob)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 64 {
		t.Fatalf("key should be a sha256 hex string, got len %d", len(key))
	}
	got, hit, err := st.Get(id)
	if err != nil || !hit || string(got) != string(blob) {
		t.Fatalf("hit=%v err=%v match=%v", hit, err, string(got) == string(blob))
	}
}

func TestMissOnUnknownIdentity(t *testing.T) {
	st, _ := cache.NewStore(t.TempDir())
	if _, hit, err := st.Get(cache.Identity{SourceID: "x", InputSHA256: "y"}); hit || err != nil {
		t.Fatalf("unknown key must miss cleanly: hit=%v err=%v", hit, err)
	}
}

func TestRevalidationFailures(t *testing.T) {
	dir := t.TempDir()
	st, _ := cache.NewStore(dir)
	id := cache.Identity{SourceID: "src", InputSHA256: "deadbeef"}
	blob := []byte("payload")
	key, _ := st.Put(id, blob)
	blobPath := filepath.Join(dir, key+".bin")

	// Tamper with cached content: digest revalidation must fail as checksum.
	tampered := append([]byte(nil), blob...)
	tampered[0] = 'X'
	if err := os.WriteFile(blobPath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Get(id); err == nil {
		t.Fatal("tampered content must fail digest revalidation")
	} else if fe, ok := fault.As(err); !ok || fe.Kind != fault.KindChecksum {
		t.Fatalf("expected checksum fault, got %v", err)
	}
	// After rejection the poisoned entry must be evicted (next Get misses).
	if _, hit, _ := st.Get(id); hit {
		t.Fatal("invalidated entry should have been evicted")
	}

	// Different source identity on the same logical input must not hit.
	other := cache.Identity{SourceID: "different-src", InputSHA256: "deadbeef"}
	if _, hit, _ := st.Get(other); hit {
		t.Fatal("source identity mismatch must invalidate")
	}
}

func TestPathTraversalRejected(t *testing.T) {
	if _, err := cache.NewStore("../escape"); err == nil {
		t.Fatal("path traversal must be rejected")
	}
}
