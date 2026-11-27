package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"avrepair/internal/fixture"
	"avrepair/internal/index"
)

func makeCases(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := fixture.Generate(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func run(t *testing.T, opts Options) (*Report, error) {
	t.Helper()
	return Run(opts)
}

func TestLossyCaseMeetsAcceptanceThresholds(t *testing.T) {
	dir := makeCases(t)
	out := t.TempDir()
	state := t.TempDir()
	rep, err := run(t, Options{Input: filepath.Join(dir, fixture.CaseLossy), StateDir: state, OutputDir: out})
	if err != nil {
		t.Fatal(err)
	}
	if rep.AVSyncMaxMS > 20 {
		t.Fatalf("av_sync_max_ms=%.3f exceeds 20", rep.AVSyncMaxMS)
	}
	if rep.LossRecoveryPct < 95 {
		t.Fatalf("loss_recovery_pct=%.3f below 95", rep.LossRecoveryPct)
	}
	if rep.P95SegmentMS > 50 {
		t.Fatalf("p95_segment_ms=%.3f exceeds 50", rep.P95SegmentMS)
	}
	if rep.IndexEntries != 96 {
		t.Fatalf("seek_index_entries=%d, want 96", rep.IndexEntries)
	}
	if rep.Quarantined != 0 {
		t.Fatalf("lossy case must fully recover, got %d quarantined", rep.Quarantined)
	}
	if _, err := os.Stat(filepath.Join(out, "index.json")); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptCaseQuarantinesExactlyOneSegment(t *testing.T) {
	dir := makeCases(t)
	out := t.TempDir()
	rep, err := run(t, Options{Input: filepath.Join(dir, fixture.CaseCorrupt), StateDir: t.TempDir(), OutputDir: out})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Quarantined != 1 {
		t.Fatalf("quarantined_segments=%d, want 1", rep.Quarantined)
	}
	data, err := os.ReadFile(filepath.Join(out, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Validate(out); err != nil {
		t.Fatalf("committed index invalid: %v", err)
	}
	if len(idx.Entries) != rep.IndexEntries {
		t.Fatalf("index entries %d != report %d", len(idx.Entries), rep.IndexEntries)
	}
}

func TestInterruptThenResumeReusesStagesAndKeepsHash(t *testing.T) {
	dir := makeCases(t)
	out := t.TempDir()
	state := t.TempDir()
	_, err := run(t, Options{Input: filepath.Join(dir, fixture.CaseNormal), StateDir: state, OutputDir: out, InterruptAt: StageIndex})
	if err == nil {
		t.Fatal("expected interrupt error")
	}
	if _, statErr := os.Stat(filepath.Join(out, "index.json")); statErr != nil {
		t.Fatalf("index must exist after interrupt-at-index: %v", statErr)
	}
	rep, err := run(t, Options{Input: filepath.Join(dir, fixture.CaseNormal), StateDir: state, OutputDir: out, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReusedStages < 3 {
		t.Fatalf("reused_stages=%d, want >= 3", rep.ReusedStages)
	}
	if rep.ResultSHA256 == "" {
		t.Fatal("result hash missing")
	}
	rep2, err := run(t, Options{Input: filepath.Join(dir, fixture.CaseNormal), StateDir: state, OutputDir: out, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.ResultSHA256 != rep.ResultSHA256 {
		t.Fatalf("result hash changed across resumes: %s != %s", rep2.ResultSHA256, rep.ResultSHA256)
	}
}

func TestRepeatedFreshRunsProduceIdenticalResult(t *testing.T) {
	dir := makeCases(t)
	var hashes []string
	for i := 0; i < 2; i++ {
		rep, err := run(t, Options{
			Input: filepath.Join(dir, fixture.CaseLossy), StateDir: t.TempDir(), OutputDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, rep.ResultSHA256)
	}
	if hashes[0] != hashes[1] {
		t.Fatalf("fresh runs diverged: %s != %s", hashes[0], hashes[1])
	}
}

func TestInputDigestMismatchDisablesReuse(t *testing.T) {
	dir := makeCases(t)
	out := t.TempDir()
	state := t.TempDir()
	copyDir := t.TempDir()
	if err := os.CopyFS(copyDir, os.DirFS(filepath.Join(dir, fixture.CaseNormal))); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, Options{Input: copyDir, StateDir: state, OutputDir: out}); err != nil {
		t.Fatal(err)
	}
	// Touch one segment so the digest changes, then resume.
	seg := filepath.Join(copyDir, "hls", "video-00001.ts")
	if err := os.WriteFile(seg, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := run(t, Options{Input: copyDir, StateDir: state, OutputDir: out, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReusedStages != 0 {
		t.Fatalf("reused_stages=%d after input change, want 0", rep.ReusedStages)
	}
}
