package index

import (
	"os"
	"path/filepath"
	"testing"

	"cinepipe/internal/synth"
)

func writeInput(t *testing.T, dir string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, "sample.ts")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCacheHitAndDeterminism(t *testing.T) {
	dir := t.TempDir()
	s := synth.Build(synth.SampleNormal, synth.Defaults(synth.SampleNormal, 42))

	first, st1, err := LoadOrBuild(dir, s.Data)
	if err != nil {
		t.Fatal(err)
	}
	if st1.Hit {
		t.Fatal("first run should build")
	}
	second, st2, err := LoadOrBuild(dir, s.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.Hit {
		t.Fatalf("second run should hit cache: %+v", st2)
	}
	if first.Source.SHA256 != second.Source.SHA256 {
		t.Fatal("hash mismatch between hits")
	}

	// Repeated builds produce identical cache file bytes.
	path := st1.Path
	h1, err := FileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	// Force a rebuild by removing the cache, then compare bytes.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrBuild(dir, s.Data); err != nil {
		t.Fatal(err)
	}
	h2, err := FileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("cache file bytes differ across rebuilds: %s vs %s", h1, h2)
	}
}

func TestCacheInvalidatesOnInputChange(t *testing.T) {
	dir := t.TempDir()
	a := synth.Build(synth.SampleNormal, synth.Defaults(synth.SampleNormal, 42))
	b := synth.Build(synth.SampleNormal, synth.Defaults(synth.SampleNormal, 43))
	if _, _, err := LoadOrBuild(dir, a.Data); err != nil {
		t.Fatal(err)
	}
	idx, st, err := LoadOrBuild(dir, b.Data)
	if err != nil {
		t.Fatal(err)
	}
	if st.Hit {
		t.Fatal("changed input must not hit the old cache entry")
	}
	if idx.Source.SHA256 != Hash(b.Data) {
		t.Fatal("index keyed to wrong input")
	}
	// The two cache files coexist, each named for its own hash.
	entries, _ := os.ReadDir(dir)
	if len(entries) < 2 {
		t.Fatalf("expected two cache files, found %d", len(entries))
	}
}

func TestCacheRebuildsTruncatedFile(t *testing.T) {
	dir := t.TempDir()
	s := synth.Build(synth.SampleNormal, synth.Defaults(synth.SampleNormal, 42))
	_, st, err := LoadOrBuild(dir, s.Data)
	if err != nil {
		t.Fatal(err)
	}
	// Truncate the cache file to simulate an interrupted write.
	data, err := os.ReadFile(st.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.Path, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	idx, st2, err := LoadOrBuild(dir, s.Data)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Hit {
		t.Fatal("truncated cache should not hit")
	}
	if !st2.Rebuilt {
		t.Fatal("truncated cache should be marked rebuilt")
	}
	if idx.Source.SHA256 != Hash(s.Data) {
		t.Fatal("rebuilt index has wrong hash")
	}
	// The rebuild replaced the truncated file with a full one.
	if _, ok, reason := load(st.Path, Hash(s.Data)); !ok {
		t.Fatalf("rebuilt cache does not validate: %s", reason)
	}
}

func TestCacheIgnoresGarbageFile(t *testing.T) {
	dir := t.TempDir()
	s := synth.Build(synth.SampleNormal, synth.Defaults(synth.SampleNormal, 42))
	path := filepath.Join(dir, cacheName(Hash(s.Data)))
	if err := os.WriteFile(path, []byte("not json at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, st, err := LoadOrBuild(dir, s.Data)
	if err != nil {
		t.Fatal(err)
	}
	if st.Hit || !st.Rebuilt {
		t.Fatalf("garbage cache should rebuild: %+v", st)
	}
	if idx.Source.SHA256 != Hash(s.Data) {
		t.Fatal("wrong hash after rebuild")
	}
}

func TestCachePreservesIndexContent(t *testing.T) {
	dir := t.TempDir()
	s := synth.Build(synth.SampleContinuity, synth.Defaults(synth.SampleContinuity, 42))
	built := Run(s.Data)
	idx, st, err := LoadOrBuild(dir, s.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Rebuilt && !st.Hit {
		// First call builds
	}
	if len(idx.Ranges) != len(built.Ranges) {
		t.Fatalf("cached ranges %d vs %d", len(idx.Ranges), len(built.Ranges))
	}
	for i := range built.Ranges {
		if idx.Ranges[i].StartByte != built.Ranges[i].StartByte ||
			idx.Ranges[i].RecoveredAt != built.Ranges[i].RecoveredAt {
			t.Fatalf("range %d differs after round-trip", i)
		}
	}
}

func TestAtomicWriteLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	s := synth.Build(synth.SampleNormal, synth.Defaults(synth.SampleNormal, 42))
	if _, _, err := LoadOrBuild(dir, s.Data); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if len(e.Name()) > 0 && e.Name()[0] == '.' {
			t.Fatalf("leftover temp file %q", e.Name())
		}
	}
}
