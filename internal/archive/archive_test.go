package archive_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"archivecheck/internal/archive"
	"archivecheck/internal/demux"
	"archivecheck/internal/media"
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

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// TestCacheKeyBinding proves every bound input changes the key.
func TestCacheKeyBinding(t *testing.T) {
	base := archive.CacheKeyInput{SourceSHA256: sha("src"), Mapping: "m", Format: demux.FormatVersion}
	variants := []archive.CacheKeyInput{
		{SourceSHA256: sha("other"), Mapping: "m", Format: demux.FormatVersion},
		{SourceSHA256: sha("src"), Mapping: "different", Format: demux.FormatVersion},
		{SourceSHA256: sha("src"), Mapping: "m", Format: "ac-bmff-2"},
	}
	k0 := archive.Key(base)
	for i, v := range variants {
		if archive.Key(v) == k0 {
			t.Fatalf("variant %d produced an identical cache key", i)
		}
	}
	if len(k0) != 64 {
		t.Fatalf("cache key not 64 hex chars: %d", len(k0))
	}
}

// TestCachePutVerifyHit is the happy path: a stored fragment verifies on hit.
func TestCachePutVerifyHit(t *testing.T) {
	root := t.TempDir()
	cache, err := archive.NewCache(root)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "source.mp4")
	writeFile(t, src, "source-bytes")
	frag := filepath.Join(root, "frag.mp4")
	writeFile(t, frag, "fragment-bytes")

	key := archive.Key(archive.CacheKeyInput{
		SourceSHA256: sha("source-bytes"), Mapping: "m", Format: demux.FormatVersion,
	})
	entry := archive.CacheEntry{
		Key: key, FragmentPath: frag, FragmentSHA: sha("fragment-bytes"),
		SourceSHA: sha("source-bytes"), Format: demux.FormatVersion, Samples: 3,
	}
	if err := cache.Put(entry); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := cache.VerifyHit(key, src)
	if err != nil {
		t.Fatalf("verified hit rejected: %v", err)
	}
	if got.Samples != 3 {
		t.Fatalf("entry samples = %d, want 3", got.Samples)
	}

	// Restore must reproduce identical bytes at the destination.
	dst := filepath.Join(root, "out", "restored.mp4")
	if err := cache.Restore(key, dst); err != nil {
		t.Fatalf("restore: %v", err)
	}
	b, _ := os.ReadFile(dst)
	if string(b) != "fragment-bytes" {
		t.Fatal("restored fragment content differs")
	}
}

// TestForgedFragmentDigestRejected covers the security requirement: a cache
// record whose fragment digest does not match the blob must be refused.
func TestForgedFragmentDigestRejected(t *testing.T) {
	root := t.TempDir()
	cache, _ := archive.NewCache(root)
	src := filepath.Join(root, "source.mp4")
	writeFile(t, src, "source-bytes")
	frag := filepath.Join(root, "frag.mp4")
	writeFile(t, frag, "actual-fragment")

	key := archive.Key(archive.CacheKeyInput{
		SourceSHA256: sha("source-bytes"), Mapping: "m", Format: demux.FormatVersion,
	})
	forged := archive.CacheEntry{
		Key: key, FragmentPath: frag,
		FragmentSHA: sha("forged-claimed-digest"), // deliberately wrong
		SourceSHA:   sha("source-bytes"), Format: demux.FormatVersion,
	}
	if err := cache.Put(forged); err != nil {
		t.Fatalf("put: %v", err)
	}
	_, err := cache.VerifyHit(key, src)
	if err == nil {
		t.Fatal("forged fragment digest must reject the hit")
	}
	f, ok := media.AsFault(err)
	if !ok || f.Kind != media.KindDigestMismatch || !f.Fatal {
		t.Fatalf("expected fatal digest-mismatch fault, got %v", err)
	}
}

// TestSourceDigestChangeRejected ensures a modified source invalidates a hit.
func TestSourceDigestChangeRejected(t *testing.T) {
	root := t.TempDir()
	cache, _ := archive.NewCache(root)
	src := filepath.Join(root, "source.mp4")
	writeFile(t, src, "v1")
	frag := filepath.Join(root, "frag.mp4")
	writeFile(t, frag, "fragment")
	key := archive.Key(archive.CacheKeyInput{SourceSHA256: sha("v1"), Mapping: "m", Format: demux.FormatVersion})
	_ = cache.Put(archive.CacheEntry{
		Key: key, FragmentPath: frag, FragmentSHA: sha("fragment"),
		SourceSHA: sha("v1"), Format: demux.FormatVersion,
	})
	writeFile(t, src, "v2-modified")
	if _, err := cache.VerifyHit(key, src); err == nil {
		t.Fatal("changed source digest must reject the cached hit")
	}
}

// TestCheckpointCommitAndResume proves an interrupted run skips already
// recovered segments (no duplicate ingestion) and keeps stable ordering.
func TestCheckpointCommitAndResume(t *testing.T) {
	root := t.TempDir()
	cp, err := archive.OpenCheckpoint(root, "batch1", demux.FormatVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.CommitRecovered(archive.SegmentState{
		Segment: "seg01", OutputPath: filepath.Join(root, "seg01.mp4"),
		FragmentSHA: sha("f1"), SourceSHA: sha("s1"), Samples: 36, Tracks: 3,
	}); err != nil {
		t.Fatal(err)
	}
	// Reopen from disk to simulate a fresh process resuming the batch.
	cp2, err := archive.OpenCheckpoint(root, "batch1", demux.FormatVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !cp2.IsDone("seg01", sha("s1")) {
		t.Fatal("committed segment must be considered done on resume")
	}
	if cp2.IsDone("seg01", sha("different-source")) {
		t.Fatal("segment with changed source digest must not be done")
	}
	if len(cp2.Snapshot()) != 1 {
		t.Fatal("checkpoint did not persist exactly one item")
	}
}

// TestCheckpointRollbackOnDigestChange covers recovery when a previously
// committed fragment's digest changed: rollback clears it for recomputation.
func TestCheckpointRollbackOnDigestChange(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "seg02.mp4")
	writeFile(t, out, "stale-output")
	cp, _ := archive.OpenCheckpoint(root, "batch2", demux.FormatVersion)
	_ = cp.CommitRecovered(archive.SegmentState{
		Segment: "seg02", OutputPath: out, FragmentSHA: sha("stale"), SourceSHA: sha("s2"),
	})
	if err := cp.Rollback("seg02"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("rollback must remove the stale output file")
	}
	if cp.IsDone("seg02", sha("s2")) {
		t.Fatal("rolled-back segment must be recomputed on resume")
	}
}

// TestCheckpointQuarantinePersists verifies unrecoverable segments are
// recorded with located faults but do not block the rest of the batch.
func TestCheckpointQuarantinePersists(t *testing.T) {
	root := t.TempDir()
	cp, _ := archive.OpenCheckpoint(root, "batch3", demux.FormatVersion)
	f := media.NewFault(media.KindUnsupported, "seg07", 1, 148, "codec vc01 unsupported")
	_ = cp.CommitQuarantined(archive.SegmentState{Segment: "seg07", Faults: []*media.Fault{f}})
	cp2, _ := archive.OpenCheckpoint(root, "batch3", demux.FormatVersion)
	if !cp2.IsQuarantined("seg07") {
		t.Fatal("quarantine must survive a checkpoint reload")
	}
	st := cp2.Get("seg07")
	if st == nil || len(st.Faults) != 1 || st.Faults[0].Offset != 148 {
		t.Fatalf("quarantined state lost its located fault: %+v", st)
	}
}
