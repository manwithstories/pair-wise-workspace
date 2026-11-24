package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"mediad/internal/fixture"
	"mediad/internal/timeline"
)

func runCfg(t *testing.T, cfg fixture.Config) *Report {
	t.Helper()
	dir := t.TempDir()
	cfg.Root = filepath.Join(dir, MediaDir)
	if err := fixture.Generate(cfg); err != nil {
		t.Fatalf("generate: %v", err)
	}
	rep, err := Execute(context.Background(), ExecuteParams{
		MediaRoot:    filepath.Join(dir, MediaDir),
		ManifestPath: filepath.Join(dir, "manifest.json"),
		Config:       Config{Workers: 2, Queue: 4},
		PathPrefix:   MediaDir, VerifyBaseDir: dir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return rep
}

func manifestHasCode(rep *Report, code string) bool {
	if rep.Manifest == nil {
		return false
	}
	for _, is := range rep.Manifest.Issues {
		if is.Code == code {
			return true
		}
	}
	return false
}

func TestMissingFragmentPropagatesGapToManifest(t *testing.T) {
	cfg := fixture.CleanConfig("", 23)
	cfg.Omit = []fixture.Loc{{Rep: "high", Index: 3}}
	rep := runCfg(t, cfg)
	if !manifestHasCode(rep, timeline.IssueGap) {
		t.Fatalf("expected sequence_gap in manifest issues: %+v", rep.Manifest.Issues)
	}
	if rep.Manifest.Stats.Gaps == 0 {
		t.Fatal("stats gaps must be > 0")
	}
}

func TestTFDTRollbackWithoutRTPOpensPeriodInManifest(t *testing.T) {
	repSpec := fixture.HighRep()
	rollback := uint64(100)
	repSpec.Hooks = map[int]fixture.FragHook{
		2: {TFDT: &rollback, NoRTP: true},
	}
	cfg := fixture.Config{Seed: 23, FragmentsPerRep: 4, Reps: []fixture.Rep{repSpec}}
	rep := runCfg(t, cfg)
	if !manifestHasCode(rep, timeline.IssueTFDTRegression) ||
		!manifestHasCode(rep, timeline.IssueNewPeriodMissingRTP) {
		t.Fatalf("expected regression + explicit new period, issues=%+v", rep.Manifest.Issues)
	}
}

func TestMetadataConflictSplitsAudioAndMarksVideoIncompatible(t *testing.T) {
	cfg := fixture.MetadataConflictConfig("", 23)
	rep := runCfg(t, cfg)

	if len(rep.Manifest.AudioGroups) < 2 {
		t.Fatalf("want >=2 audio groups, got %d", len(rep.Manifest.AudioGroups))
	}
	foundLow := false
	for _, r := range rep.Manifest.IncompatibleVideo {
		if r == "low" {
			foundLow = true
		}
	}
	if !foundLow {
		t.Fatalf("low must be video-incompatible: %+v", rep.Manifest.IncompatibleVideo)
	}
}

func TestDigestDuplicateCollapsesButStaysVerified(t *testing.T) {
	cfg := fixture.CleanConfig("", 23)
	cfg.ExtraDuplicate = true
	rep := runCfg(t, cfg)
	if !manifestHasCode(rep, timeline.IssueDuplicateDigest) {
		t.Fatalf("expected duplicate_digest issue: %+v", rep.Manifest.Issues)
	}
	if rep.Manifest.Stats.Fragments != 25 {
		t.Errorf("physical fragments=%d want 25", rep.Manifest.Stats.Fragments)
	}
	if rep.Manifest.Stats.Unique != 24 {
		t.Errorf("unique=%d want 24", rep.Manifest.Stats.Unique)
	}
	if !rep.Verify.Complete() {
		t.Fatalf("verification must stay complete: %+v", rep.Verify)
	}
}
