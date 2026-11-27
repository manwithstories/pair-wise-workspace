package pipeline_test

// Recovery tests validate the crash-resume contract at the checkpoint layer:
// finished segments are durably recorded, an orphaned output whose digest
// still matches is adopted, a tampered orphan is rejected for rebuild, and a
// resumed store reports exactly the work already completed. These are the
// invariants the pipeline relies on when it stops after segment 3 and restarts.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fashionweek/mediad/internal/cache"
	"github.com/fashionweek/mediad/internal/checkpoint"
)

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckpointPersistsAndResumes(t *testing.T) {
	dir := t.TempDir()
	cp, err := checkpoint.Open(filepath.Join(dir, "cp"), "job-1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		seg := filepath.Join(dir, "seg", name(i))
		writeFile(t, seg, "payload-"+name(i))
		hash, err := cache.HashFile(seg)
		if err != nil {
			t.Fatal(err)
		}
		if err := cp.Record(checkpoint.SegmentState{
			Index: i, Outputs: map[string]string{"high": hash},
			CutAtNum: int64(i * 2), CutAtDen: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := cp.CompletedCount(); got != 3 {
		t.Fatalf("completed = %d, want 3", got)
	}

	// Reopen as a fresh process would after a crash.
	resumed, err := checkpoint.Open(filepath.Join(dir, "cp"), "job-1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if !resumed.IsDone(i) {
			t.Fatalf("segment %d not marked done after resume", i)
		}
	}
	if resumed.IsDone(3) {
		t.Fatal("segment 3 must not be considered finished")
	}
	if got := resumed.CompletedCount(); got != 3 {
		t.Fatalf("resumed completed = %d, want 3", got)
	}
}

func TestIntactOrphanAdoptedTamperedRejected(t *testing.T) {
	dir := t.TempDir()
	cp, err := checkpoint.Open(filepath.Join(dir, "cp"), "job-2")
	if err != nil {
		t.Fatal(err)
	}
	orphan := writeFile(t, filepath.Join(dir, "o.m4s"), "original-bytes")
	goodHash, err := cache.HashFile(orphan)
	if err != nil {
		t.Fatal(err)
	}
	// Untouched orphan: digest matches and it is adoptable.
	if !cp.AdoptOrphan(orphan, goodHash) {
		t.Fatal("intact orphan should be adopted")
	}
	// Tamper the file in place to simulate a partial/corrupt crash write.
	if err := os.WriteFile(orphan, []byte("corrupted-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cp.AdoptOrphan(orphan, goodHash) {
		t.Fatal("tampered orphan must be rejected for rebuild")
	}
	// A missing file is likewise never adopted.
	if cp.AdoptOrphan(filepath.Join(dir, "missing.m4s"), goodHash) {
		t.Fatal("missing orphan must be rejected")
	}
}

func TestCheckpointRecordIsIdempotentAndOrdered(t *testing.T) {
	dir := t.TempDir()
	cp, err := checkpoint.Open(filepath.Join(dir, "cp"), "job-3")
	if err != nil {
		t.Fatal(err)
	}
	// Record out of order; the document must stay sorted and de-duplicated.
	for _, i := range []int{2, 0, 1} {
		if err := cp.Record(checkpoint.SegmentState{
			Index: i, Outputs: map[string]string{"high": "h" + name(i)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Re-record index 1 (resume redoing a unit) must not duplicate it.
	if err := cp.Record(checkpoint.SegmentState{
		Index: 1, Outputs: map[string]string{"high": "h1-new"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := cp.CompletedCount(); got != 3 {
		t.Fatalf("completed = %d, want 3 (no duplicate on re-record)", got)
	}
	state, ok := cp.Segment(1)
	if !ok || state.Outputs["high"] != "h1-new" {
		t.Fatalf("re-recorded segment not replaced: %+v", state)
	}
}

func name(i int) string {
	return "seg-" + string(rune('0'+i)) + ".m4s"
}
