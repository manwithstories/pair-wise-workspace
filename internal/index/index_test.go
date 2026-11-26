package index

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediaflow/internal/fixture"
	"mediaflow/internal/timeline"
)

func buildClip(t *testing.T, seed int64) *fixture.Clip {
	t.Helper()
	clip, err := fixture.Generate(fixture.DefaultConfig(seed))
	if err != nil {
		t.Fatal(err)
	}
	return clip
}

func TestScanDeterministic(t *testing.T) {
	clip := buildClip(t, 41)
	sha, err := HashClip(clip)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Scan(clip, sha)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Scan(clip, sha)
	if err != nil {
		t.Fatal(err)
	}
	if a.ContentHash() != b.ContentHash() {
		t.Fatal("identical input produced different index content hash")
	}
	if a.InputSHA != sha || a.Version != IndexVersion {
		t.Fatal("index metadata wrong")
	}
	if a.Packets <= 0 || len(a.Tracks) < 4 || a.SpanUS <= 0 {
		t.Fatalf("scan summary empty: %+v", a)
	}
}

func TestCacheHitAndRebuild(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	clip := buildClip(t, 41)
	sha, _ := HashClip(clip)

	first, rebuilt, err := cache.GetOrScan(clip, sha)
	if err != nil || !rebuilt {
		t.Fatalf("first call must rebuild: rebuilt=%v err=%v", rebuilt, err)
	}
	second, rebuilt, err := cache.GetOrScan(clip, sha)
	if err != nil || rebuilt {
		t.Fatalf("second call must hit cache: rebuilt=%v err=%v", rebuilt, err)
	}
	if first.ContentHash() != second.ContentHash() {
		t.Fatal("cache returned inconsistent index")
	}
}

func TestCacheRejectsChangedInput(t *testing.T) {
	dir := t.TempDir()
	cache, _ := NewCache(dir)
	clip := buildClip(t, 41)
	sha, _ := HashClip(clip)
	if _, _, err := cache.GetOrScan(clip, sha); err != nil {
		t.Fatal(err)
	}
	// A different input hash must be treated as a miss, not served stale data.
	other, _, err := cache.GetOrScan(clip, strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("changed input should trigger rebuild, got %v", err)
	}
	if other.InputSHA != strings.Repeat("a", 64) {
		t.Fatal("rebuilt index recorded wrong input hash")
	}
}

func TestCacheCorruptionDetected(t *testing.T) {
	dir := t.TempDir()
	cache, _ := NewCache(dir)
	clip := buildClip(t, 41)
	sha, _ := HashClip(clip)
	idx, _, _ := cache.GetOrScan(clip, sha)

	// Tamper the persisted envelope's content checksum.
	path := filepath.Join(dir, "index.json")
	raw, _ := os.ReadFile(path)
	tampered := strings.Replace(string(raw), `"content_sha256": "`, `"content_sha256": "deadbeef`, 1)
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Load(sha); !errors.Is(err, timeline.ErrCorruptIndex) {
		t.Fatalf("tampered cache not rejected: %v", err)
	}

	// Version drift must also invalidate.
	idx.Version = IndexVersion - 1
	if err := cache.Save(idx, sha); err == nil {
		t.Fatal("stale-version save must be rejected")
	}
}

func TestPathEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	cache, _ := NewCache(dir)
	if _, err := safeJoin(cache.root, "../../etc/passwd"); !errors.Is(err, timeline.ErrPathEscape) {
		t.Fatalf("traversal not rejected: %v", err)
	}
	good, err := safeJoin(cache.root, filepath.Join("sub", "index.json"))
	if err != nil || !filepath.IsAbs(good) {
		t.Fatalf("safe join failed: %v %s", err, good)
	}
}

func TestHashReaderSizeLimit(t *testing.T) {
	big := strings.NewReader(strings.Repeat("x", MaxInputBytes+10))
	if _, _, err := HashReader(big); !errors.Is(err, timeline.ErrLimit) {
		t.Fatalf("oversized input not rejected: %v", err)
	}
	small := strings.NewReader("deterministic")
	sha, n, err := HashReader(small)
	if err != nil || n != 13 || len(sha) != 64 {
		t.Fatalf("small hash wrong: %v n=%d sha=%q", err, n, sha)
	}
}

func TestTruncatedEventIndexed(t *testing.T) {
	clip := buildClip(t, 41)
	clip.Spec.TruncateEvents = []int{0, 2}
	damaged := fixture.ApplyDamage(clip)
	sha, _ := HashClip(damaged)
	idx, err := Scan(damaged, sha)
	if err != nil {
		t.Fatal(err)
	}
	var truncated int
	var firstOffset int64 = -1
	for _, ev := range idx.Events {
		if ev.Truncated {
			truncated++
			if firstOffset < 0 {
				firstOffset = ev.Offset
			}
		}
	}
	if truncated != 2 || firstOffset != idx.Events[0].Offset {
		t.Fatalf("truncation indexing wrong: count=%d offset=%d", truncated, firstOffset)
	}
}
