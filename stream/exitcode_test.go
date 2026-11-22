package stream

import (
	"os"
	"path/filepath"
	"testing"

	"mediaslice/media"
)

// TestResumeFailureKindIsCheckpoint confirms the resume-rejection error maps
// to the corrupt-input exit code (2) via its media error kind.
func TestResumeFailureKindIsCheckpoint(t *testing.T) {
	dir := t.TempDir()
	h := buildHarness(t, dir, []string{"800k"}, 4)
	cfg := DefaultConfig()
	cfg.CrashAfterSequence = 1
	cfg.CheckpointEvery = 1
	r, _ := NewRunner(dir, h.plan, h.indexes, h.bounds, cfg)
	if _, err := r.Run(); err == nil {
		t.Fatal("expected crash")
	}
	// Tamper the recorded checksum so resume validation must fail.
	cp, err := LoadCheckpoint(dir)
	if err != nil || cp == nil {
		t.Fatalf("checkpoint load: %v", err)
	}
	for k, e := range cp.Completed {
		e.SHA256 = "deadbeef"
		cp.Completed[k] = e
		break
	}
	if err := SaveCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}
	r2, _ := NewRunner(dir, h.plan, h.indexes, h.bounds, DefaultConfig())
	_, err = r2.Run()
	me, ok := err.(*media.MediaError)
	if !ok {
		t.Fatalf("expected media error, got %T: %v", err, err)
	}
	if me.Kind != media.KindCheckpoint {
		t.Fatalf("kind=%s want checkpoint", me.Kind)
	}
	// And the segment file still physically exists (last complete segment
	// preserved), proving no partial segment was committed.
	if _, err := os.Stat(filepath.Join(dir, segmentFileName("800k", 1))); err != nil {
		t.Fatalf("completed segment lost: %v", err)
	}
}
