package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func runOpts(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{
		Seed:      41,
		DropEvery: 97,
		CacheDir:  filepath.Join(dir, "cache"),
		DataDir:   dir,
	}
}

func TestRunProducesInvariants(t *testing.T) {
	opt := runOpts(t)
	sum, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	if sum.AVSyncMs > 20 {
		t.Fatalf("AV_SYNC_MS=%d", sum.AVSyncMs)
	}
	if sum.P95LatencyMs > 500 || sum.P95LatencyMs < 1 {
		t.Fatalf("P95=%d", sum.P95LatencyMs)
	}
	if sum.LossRecoveryPct < 95 {
		t.Fatalf("recovery=%v", sum.LossRecoveryPct)
	}
	if sum.IndexPoints <= 0 || !sum.IndexValid {
		t.Fatalf("index points=%d valid=%t", sum.IndexPoints, sum.IndexValid)
	}
	if len(sum.SourceSHA256) != 64 || len(sum.OutputSHA256) != 64 {
		t.Fatalf("digests: %q %q", sum.SourceSHA256, sum.OutputSHA256)
	}
	if !sum.SourceMatch {
		t.Fatal("source digest mismatch")
	}
}

func TestRerunIsDeterministic(t *testing.T) {
	opt := runOpts(t)
	a, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	if a.OutputSHA256 != b.OutputSHA256 {
		t.Fatalf("output digest changed: %s vs %s", a.OutputSHA256, b.OutputSHA256)
	}
	if b.CacheHits == 0 {
		t.Fatal("a second run should hit the cache")
	}
}

func TestInterruptThenResumeMatchesUninterrupted(t *testing.T) {
	dir := t.TempDir()
	base := Options{Seed: 41, DropEvery: 97, CacheDir: filepath.Join(dir, "cache"), DataDir: dir}

	straight, err := Run(base)
	if err != nil {
		t.Fatal(err)
	}

	// A fresh directory so the earlier run's checkpoint cannot leak in.
	dir2 := t.TempDir()
	opts := Options{Seed: 41, DropEvery: 97, CacheDir: filepath.Join(dir2, "cache"), DataDir: dir2,
		InterruptAfter: StageIndex, Resume: true}
	resumed, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.OutputSHA256 != straight.OutputSHA256 {
		t.Fatalf("resumed digest %s != straight %s", resumed.OutputSHA256, straight.OutputSHA256)
	}
	if resumed.IndexPoints != straight.IndexPoints {
		t.Fatalf("resumed points %d != %d", resumed.IndexPoints, straight.IndexPoints)
	}
}

func TestDamagedRunReportsErrors(t *testing.T) {
	dir := t.TempDir()
	opt := Options{Seed: 41, DropEvery: 97, Damaged: true,
		CacheDir: filepath.Join(dir, "cache"), DataDir: dir}
	sum, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	if sum.ErrorSegments <= 0 {
		t.Fatalf("ERROR_SEGMENTS=%d", sum.ErrorSegments)
	}
}

func TestCacheInvalidatedOnTamper(t *testing.T) {
	dir := t.TempDir()
	cache, err := OpenCache(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	a := Artifact{Stage: StageRepair, SourceID: "s", InputDigest: "i", ParamsDigest: "p", Payload: []byte("data")}
	if _, err := cache.Store(a); err != nil {
		t.Fatal(err)
	}
	var hits int
	if _, err := cache.Load(StageRepair, "s", "i", "p", &hits); err != nil {
		t.Fatalf("expected hit: %v", err)
	}
	// Corrupt the payload on disk; Load must reject it.
	key := Key("s", "i", "p")
	path := cache.path(StageRepair, key)
	if err := os.WriteFile(path, []byte(`{"stage":"repair","source_id":"s","input_digest":"i","params_digest":"p","content_digest":"deadbeef","payload":"ZGF0YQ=="}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Load(StageRepair, "s", "i", "p", &hits); !errors.Is(err, ErrCacheInvalid) {
		t.Fatalf("tampered entry should be invalid, got %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalidated entry should be removed")
	}
}

func TestCacheRejectsIdentityMismatch(t *testing.T) {
	dir := t.TempDir()
	cache, err := OpenCache(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	a := Artifact{Stage: StageProbe, SourceID: "s", InputDigest: "i", ParamsDigest: "p", Payload: []byte("x")}
	if _, err := cache.Store(a); err != nil {
		t.Fatal(err)
	}
	// Loading the same key but a different source must miss.
	key := Key("s", "i", "p")
	path := cache.path(StageProbe, key)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Load(StageProbe, "other", "i", "p", nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("different source must not hit: %v", err)
	}
}

func TestCheckpointRunningStageNotSkipped(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenCheckpoints(dir)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := store.Load("run")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Begin(cp, StageRepair, "in", "par"); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("run")
	if err != nil {
		t.Fatal(err)
	}
	stage, done := loaded.ResumePoint(map[string][2]string{
		StageManifest: {"in", "par"},
		StageRepair:   {"in", "par"},
		StageProbe:    {"in", "par"},
		StageIndex:    {"in", "par"},
	})
	if stage != StageManifest {
		t.Fatalf("resume point=%q, want manifest first (none verified)", stage)
	}
	if len(done) != 0 {
		t.Fatalf("done=%v", done)
	}
	if loaded.Verified(StageRepair, "in", "par") {
		t.Fatal("a running stage must not count as verified")
	}
}

func TestCheckpointDiscardsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenCheckpoints(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := store.file("run")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp, err := store.Load("run")
	if err != nil {
		t.Fatal(err)
	}
	if len(cp.Stages) != 0 {
		t.Fatal("corrupt checkpoint must be discarded")
	}
}

func TestAtomicWriteLeavesNoPartialArtifact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	if err := writeAtomic(path, []byte("complete")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "out.json" {
			t.Fatalf("stray temporary file %q", e.Name())
		}
	}
}
