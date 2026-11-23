package pipeline

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResumeAfterCompletedPriorRun covers a work directory that already
// holds a complete run and is then interrupted and resumed. Duplicate
// checkpoint records for the same stage must not corrupt resume, and the
// final hash must still match the clean reference.
func TestResumeAfterCompletedPriorRun(t *testing.T) {
	refDir := t.TempDir()
	base := Config{Seed: 8, Segments: 2, SegmentDurNS: 2_000_000_000,
		PacketRate: 50, Faults: Faults{Drop: true}}
	refCfg := base
	refCfg.WorkDir = refDir
	ref, err := Run(refCfg)
	if err != nil {
		t.Fatalf("reference: %v", err)
	}

	dir := t.TempDir()
	cfg := base
	cfg.WorkDir = dir
	if _, err := Run(cfg); err != nil {
		t.Fatalf("first complete run: %v", err)
	}
	// Final was committed by the first run, so it is durable and must
	// survive. A later crash on a recomputed stage must not remove it.
	before, err := os.Stat(filepath.Join(dir, "final.json"))
	if err != nil {
		t.Fatalf("first run left no final: %v", err)
	}
	cfg.Inject = InjectCommitCrash
	cfg.InjectStage = "sync"
	if _, err := Run(cfg); err == nil {
		t.Fatalf("expected the sync-stage crash")
	}
	after, err := os.Stat(filepath.Join(dir, "final.json"))
	if err != nil {
		t.Fatalf("a crash on a recomputed stage removed a durable final: %v", err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("durable final changed size after a crash elsewhere")
	}
	cfg.Inject = InjectNone
	cfg.InjectStage = ""
	cfg.Resume = true
	cfg.ReferenceHash = ref.NormalHash
	res, err := Run(cfg)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !res.ResumeChecked || !res.ResumeHashEqual {
		t.Fatalf("resume mismatch: got %s want %s", res.ResumedHash, ref.NormalHash)
	}
}
