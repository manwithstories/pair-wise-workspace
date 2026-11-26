package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/archive/tsrestore/internal/cache"
	"github.com/archive/tsrestore/internal/checkpoint"
	"github.com/archive/tsrestore/internal/sample"
)

// fullInputs builds the two-rendition undamaged fixture for the given length.
// An undamaged, deterministic stream guarantees identical segment bytes across
// an interrupted run, a resumed run and an independent reference run.
func fullInputs(t *testing.T, seed int64, secs int) []*RenditionInput {
	t.Helper()
	var ins []*RenditionInput
	for i, r := range sample.DefaultRenditions() {
		stream, _, err := sample.BuildRendition(r, sample.ProgramDuration(secs),
			seed+int64(i), sample.DamageConfig{})
		if err != nil {
			t.Fatal(err)
		}
		ins = append(ins, &RenditionInput{Name: r.Name, Datagrams: stream})
	}
	return ins
}

// TestInterruptedRunResumesWithoutReSequencing runs the same full stream twice:
// phase 1 halts deterministically after a fixed number of durable commits
// (clean operator interrupt), phase 2 resumes against the durable checkpoint.
// The resumed run must reproduce the complete segment set with hashes matching
// an independent reference run and no re-sequenced indices.
func TestInterruptedRunResumesWithoutReSequencing(t *testing.T) {
	dir := t.TempDir()
	const secs = 30
	ins := fullInputs(t, 41, secs)

	// Phase 1: deterministic interrupt after 2 durable commits.
	cas1, err := cache.Open(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	ckpt1, err := checkpoint.Open(filepath.Join(dir, "ckpt.json"), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), ins, ckpt1, cas1, Config{
		OutDir: dir, Seed: 41, ProgramSecs: secs, InterruptAfter: 2,
	})
	if err != context.Canceled {
		t.Fatalf("interrupt run should return Canceled, got %v", err)
	}
	committedBefore := ckpt1.Snapshot()
	committed := 0
	for _, prog := range committedBefore.Renditions {
		committed += prog.NextSegmentIndex
	}
	if committed != 2 {
		t.Fatalf("interrupt should leave exactly 2 commits, got %d", committed)
	}

	// Phase 2: reopen durable stores and resume with the same full stream.
	cas2, err := cache.Open(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	ckpt2, err := checkpoint.Open(filepath.Join(dir, "ckpt.json"), []byte("secret"))
	if err != nil {
		t.Fatalf("resume checkpoint must verify and load: %v", err)
	}
	res, err := Run(context.Background(), ins, ckpt2, cas2, Config{
		OutDir: dir, Seed: 41, ProgramSecs: secs, Resume: true,
	})
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}

	// Complete set: 2 renditions x (30 s / 6 s) segments.
	const expected = 2 * (secs / sample.SegmentSeconds)
	total := 0
	for _, rr := range res.Renditions {
		total += len(rr.Segments)
	}
	if total != expected {
		t.Fatalf("resumed segments = %d, want %d", total, expected)
	}

	// Cursors only move forward; no index is re-sequenced.
	after := ckpt2.Snapshot()
	for name, prog := range after.Renditions {
		if before := committedBefore.Renditions[name]; before != nil &&
			prog.NextSegmentIndex < before.NextSegmentIndex {
			t.Fatalf("%s cursor moved backwards on resume", name)
		}
	}

	// Every resumed segment hash must match an independent clean reference run.
	ref := referenceHashes(t, ins, secs)
	got := map[string]string{}
	for _, rr := range res.Renditions {
		for _, s := range rr.Segments {
			got[s.ID] = s.SHA256
		}
	}
	if len(got) != len(ref) {
		t.Fatalf("resumed segment count %d != reference %d", len(got), len(ref))
	}
	for id, want := range ref {
		if got[id] != want {
			t.Fatalf("resumed hash mismatch on %s", id)
		}
	}
}

// referenceHashes runs the same inputs in an isolated directory and returns the
// segment id -> hash mapping of an uninterrupted run.
func referenceHashes(t *testing.T, ins []*RenditionInput, secs int) map[string]string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ref")
	cas, err := cache.Open(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	ckpt, err := checkpoint.Open(filepath.Join(dir, "ckpt.json"), []byte("other"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), ins, ckpt, cas,
		Config{OutDir: dir, Seed: 41, ProgramSecs: secs})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, rr := range res.Renditions {
		for _, s := range rr.Segments {
			out[s.ID] = s.SHA256
		}
	}
	return out
}
