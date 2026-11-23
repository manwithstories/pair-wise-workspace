package main

// End-to-end invariants: an interrupted run resumed from its WAL must
// produce byte-identical output digests to an uninterrupted run, and the
// corrupt sample must be located to its segment and time interval.

import (
	"path/filepath"
	"testing"

	"medrelay/internal/journal"
	"medrelay/internal/txn"
)

func runPipeline(t *testing.T, workdir, interruptAfter string) (*txn.Runner, error) {
	t.Helper()
	w, err := journal.Open(filepath.Join(workdir, "wal.log"), 512)
	if err != nil {
		t.Fatal(err)
	}
	r := &txn.Runner{
		WAL:            w,
		Root:           filepath.Join(workdir, "stages"),
		InterruptAfter: interruptAfter,
	}
	err = r.Execute(pipelineStages(workdir))
	w.Close()
	return r, err
}

func TestEndToEndDeterminismAndRecovery(t *testing.T) {
	base := t.TempDir()

	// Uninterrupted reference run.
	full := filepath.Join(base, "full")
	if err := generateSample(full, 4, true); err != nil {
		t.Fatal(err)
	}
	if _, err := runPipeline(t, full, ""); err != nil {
		t.Fatal(err)
	}
	want := reportDigest(full)
	if want == "unavailable" {
		t.Fatal("reference run produced no report")
	}

	// Interrupted run, then resumed from the WAL.
	cut := filepath.Join(base, "cut")
	if err := generateSample(cut, 4, true); err != nil {
		t.Fatal(err)
	}
	if _, err := runPipeline(t, cut, "normalize"); err != txn.ErrInterrupt {
		t.Fatalf("want ErrInterrupt, got %v", err)
	}
	r, err := runPipeline(t, cut, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := reportDigest(cut); got != want {
		t.Fatalf("digest changed across interrupt/resume:\n%s\n%s", got, want)
	}
	// Only the uncommitted suffix was redone; normalize's partial output
	// was rolled back exactly once.
	if len(r.Replayed) != 5 || r.Replayed[0] != "normalize" {
		t.Fatalf("replayed %v, want 5 stages starting at normalize", r.Replayed)
	}
	if r.Rollbacks != 1 {
		t.Fatalf("rollbacks %d, want 1", r.Rollbacks)
	}
	if err := r.Verify(pipelineStages(cut)); err != nil {
		t.Fatal(err)
	}
}

func TestEndToEndMetrics(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	if err := generateSample(dir, 4, true); err != nil {
		t.Fatal(err)
	}
	if _, err := runPipeline(t, dir, ""); err != nil {
		t.Fatal(err)
	}
	sum := readSummary(dir)
	if sum == nil {
		t.Fatal("no summary")
	}
	if sum.FirstErrorSegment != "seg-3.m4s" {
		t.Fatalf("first_error_segment=%q, want seg-3.m4s", sum.FirstErrorSegment)
	}
	if sum.PacketRecoveryRate < 0.95 {
		t.Fatalf("recovery rate %f < 0.95", sum.PacketRecoveryRate)
	}
	if sum.AVSyncErrorMs > 20 {
		t.Fatalf("av sync p95 %f > 20ms", sum.AVSyncErrorMs)
	}
	if sum.SilenceIntervals == 0 {
		t.Fatal("expected the generated silent stretch to be detected")
	}
	if sum.UnrecoverableGaps != 0 {
		t.Fatalf("burst of 4 with stride 8 must fully recover, gaps=%d", sum.UnrecoverableGaps)
	}
}

func TestCorruptSegmentLocated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	if err := generateSample(dir, 4, true); err != nil {
		t.Fatal(err)
	}
	if _, err := runPipeline(t, dir, ""); err != nil {
		t.Fatal(err)
	}
	var quar quarantineOut
	if err := readJSON(stageDir(dir, "manifest"), "quarantine.json", &quar); err != nil {
		t.Fatal(err)
	}
	if len(quar.Items) != 1 {
		t.Fatalf("want 1 quarantined segment, got %+v", quar.Items)
	}
	q := quar.Items[0]
	if q.Segment != "seg-3.m4s" || q.Code != "truncated_segment" {
		t.Fatalf("bad quarantine record %+v", q)
	}
	// The failure is located to the segment's time interval [18s, 24s].
	if q.StartMs != 18000 || q.EndMs != 24000 {
		t.Fatalf("bad interval %+v", q)
	}
}
