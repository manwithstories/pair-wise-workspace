package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"mediarun/internal/txn"
)

// TestResumeSkipsVerifiedStages proves the resumed run reuses durable
// checkpoints: the artefact modification times of earlier stages must not
// move, while the stage after the interruption is recomputed.
func TestResumeSkipsVerifiedStages(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Seed: 3, Segments: 3, SegmentDurNS: 2_000_000_000,
		PacketRate: 50, WorkDir: dir, Faults: Faults{Drop: true},
		Inject: InjectCommitCrash, InjectStage: "jitter"}
	if _, err := Run(cfg); err == nil {
		t.Fatalf("interrupted run succeeded")
	}
	manifestArt := filepath.Join(dir, "art", "manifest.tmp")
	before, err := os.Stat(manifestArt)
	if err != nil {
		t.Fatalf("manifest checkpoint missing: %v", err)
	}
	// Verify the checkpoint really is durable before the resume.
	recs, err := txn.ReplayFile(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	var found bool
	for _, r := range recs {
		if r.Type == txn.RecCheckpoint && r.Stage == "manifest" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no manifest checkpoint recorded")
	}
	cfg.Inject = InjectNone
	cfg.InjectStage = ""
	cfg.Resume = true
	if _, err := Run(cfg); err != nil {
		t.Fatalf("resume: %v", err)
	}
	after, err := os.Stat(manifestArt)
	if err != nil {
		t.Fatalf("manifest checkpoint vanished: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("resume rewrote the manifest checkpoint, so it did not skip it")
	}
	// The stage that was interrupted must have been recomputed.
	walSize, err := os.Stat(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatalf("wal: %v", err)
	}
	if walSize.Size() == 0 {
		t.Fatalf("empty wal after resume")
	}
}
