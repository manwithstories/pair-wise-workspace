package pipeline_test

import (
	"os"
	"path/filepath"
	"testing"

	"archivecheck/internal/fixtgen"
	"archivecheck/internal/pipeline"
)

func batchItems(root string, entries []fixtgen.ManifestEntry) []pipeline.BatchItem {
	items := make([]pipeline.BatchItem, len(entries))
	for i, e := range entries {
		items[i] = pipeline.BatchItem{ID: e.ID, SourcePath: filepath.Join(root, "sources", e.Source)}
	}
	return items
}

// TestFullBatchRecovery is the end-to-end happy/failure path: 12 segments,
// the unsupported-codec one is quarantined and every other one recovers.
func TestFullBatchRecovery(t *testing.T) {
	root := t.TempDir()
	entries, err := fixtgen.GenerateAll(root)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := pipeline.NewRunner(root, "batch-a")
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.Run(batchItems(root, entries))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Stats.Total != fixtgen.SegmentCount {
		t.Fatalf("total = %d, want %d", res.Stats.Total, fixtgen.SegmentCount)
	}
	if res.Stats.Quarantined != 1 {
		t.Fatalf("quarantined = %d, want 1", res.Stats.Quarantined)
	}
	if res.Stats.Recovered != fixtgen.SegmentCount-1 {
		t.Fatalf("recovered = %d, want %d", res.Stats.Recovered, fixtgen.SegmentCount-1)
	}
	if res.Stats.RoundtripOK != fixtgen.SegmentCount-1 {
		t.Fatalf("roundtrip ok = %d, want %d", res.Stats.RoundtripOK, fixtgen.SegmentCount-1)
	}
	// Every recovered fragment must carry a 64-hex-char SHA-256 and exist.
	for _, sr := range res.Segments {
		if sr.Status == "quarantined" {
			if len(sr.Faults) == 0 {
				t.Fatal("quarantined segment lost its located fault")
			}
			continue
		}
		if len(sr.FragmentSHA) != 64 {
			t.Fatalf("segment %s digest not 64 hex chars", sr.Segment)
		}
		if _, err := os.Stat(filepath.Join(root, "fragments", sr.Segment+".mp4")); err != nil {
			t.Fatalf("fragment missing on disk: %v", err)
		}
	}
}

// TestResumeNoDuplicatesAndStableHashes covers the recovery path: a second run
// against the same checkpoint yields cache hits, zero duplicates and identical
// fragment digests.
func TestResumeNoDuplicatesAndStableHashes(t *testing.T) {
	root := t.TempDir()
	entries, _ := fixtgen.GenerateAll(root)

	first, err := pipeline.NewRunner(root, "batch-b")
	if err != nil {
		t.Fatal(err)
	}
	r1, err := first.Run(batchItems(root, entries))
	if err != nil {
		t.Fatal(err)
	}
	firstHashes := map[string]string{}
	for _, sr := range r1.Segments {
		firstHashes[sr.Segment] = sr.FragmentSHA
	}

	// New runner = new process, same checkpoint and cache.
	second, err := pipeline.NewRunner(root, "batch-b")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := second.Run(batchItems(root, entries))
	if err != nil {
		t.Fatal(err)
	}
	if r2.Stats.Duplicates != 0 {
		t.Fatalf("duplicates = %d, must stay zero on resume", r2.Stats.Duplicates)
	}
	if r2.Stats.CacheHits != fixtgen.SegmentCount-1 {
		t.Fatalf("cache hits = %d, want %d", r2.Stats.CacheHits, fixtgen.SegmentCount-1)
	}
	for _, sr := range r2.Segments {
		if sr.Status == "quarantined" {
			continue
		}
		if !sr.CacheHit {
			t.Fatalf("segment %s was not served as a cache hit on resume", sr.Segment)
		}
		if sr.FragmentSHA != firstHashes[sr.Segment] {
			t.Fatalf("segment %s digest changed across runs", sr.Segment)
		}
	}
}

// TestQuarantineDoesNotBlockBatch ensures a bad segment leaves all siblings
// recoverable and the batch still completes successfully.
func TestQuarantineDoesNotBlockBatch(t *testing.T) {
	root := t.TempDir()
	entries, _ := fixtgen.GenerateAll(root)
	runner, _ := pipeline.NewRunner(root, "batch-c")
	res, err := runner.Run(batchItems(root, entries))
	if err != nil {
		t.Fatalf("a quarantined segment must not fail the whole batch: %v", err)
	}
	quarantined := ""
	for _, sr := range res.Segments {
		if sr.Status == "quarantined" {
			quarantined = sr.Segment
		}
	}
	if quarantined == "" {
		t.Fatal("expected one quarantined segment")
	}
	if res.Stats.Recovered+res.Stats.Quarantined != fixtgen.SegmentCount {
		t.Fatal("every segment must be accounted for exactly once")
	}
}

// TestForgedCacheForcesRecompute is the digest-failure path: invalidating one
// segment's cache and forcing recompute yields one fewer cache hit while the
// recomputed fragment keeps the same deterministic digest as the first run.
func TestForgedCacheForcesRecompute(t *testing.T) {
	root := t.TempDir()
	entries, _ := fixtgen.GenerateAll(root)
	runner, _ := pipeline.NewRunner(root, "batch-d")
	first, err := runner.Run(batchItems(root, entries))
	if err != nil {
		t.Fatal(err)
	}
	firstHash := map[string]string{}
	for _, sr := range first.Segments {
		firstHash[sr.Segment] = sr.FragmentSHA
	}

	// Target a clean, recoverable segment and tamper with its cached entry.
	target := entries[0].ID
	if err := runner.ForceRecomputeSegment(target); err != nil {
		t.Fatalf("force recompute: %v", err)
	}
	second, err := runner.Run(batchItems(root, entries))
	if err != nil {
		t.Fatalf("recompute must not fail the batch: %v", err)
	}
	// All other recovered segments stay cache hits; the target is recomputed.
	if second.Stats.CacheHits != first.Stats.Recovered-1 {
		t.Fatalf("cache hits = %d, want %d after one recompute",
			second.Stats.CacheHits, first.Stats.Recovered-1)
	}
	for _, sr := range second.Segments {
		if sr.Segment == target {
			if sr.CacheHit {
				t.Fatal("tampered segment must not be served as a cache hit")
			}
			if sr.FragmentSHA != firstHash[target] {
				t.Fatal("recomputed fragment digest must match the deterministic original")
			}
		}
	}
	if second.Stats.Duplicates != 0 {
		t.Fatal("forced recompute must not duplicate any sample")
	}
}
