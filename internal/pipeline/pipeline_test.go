package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"mediad/internal/fixture"
)

func genTree(t *testing.T, cfg fixture.Config) string {
	t.Helper()
	dir := t.TempDir()
	cfg.Root = filepath.Join(dir, MediaDir)
	if err := fixture.Generate(cfg); err != nil {
		t.Fatalf("generate: %v", err)
	}
	return dir
}

func execDir(t *testing.T, dir, caseName string, cfg Config) *Report {
	t.Helper()
	rep, err := Execute(context.Background(), ExecuteParams{
		MediaRoot:    filepath.Join(dir, MediaDir),
		ManifestPath: filepath.Join(dir, "manifest.json"),
		Case:         caseName, Seed: 23, Config: cfg,
		PathPrefix: MediaDir, VerifyBaseDir: dir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return rep
}

func TestCleanAcceptanceCounts(t *testing.T) {
	dir := genTree(t, fixture.CleanConfig("", 23))
	rep := execDir(t, dir, "clean", Config{Workers: 4, Queue: 8})

	if rep.ExitCode != 0 {
		t.Fatalf("exit=%d want 0", rep.ExitCode)
	}
	st := rep.Manifest.Stats
	if st.Fragments != 24 {
		t.Errorf("fragments=%d want 24", st.Fragments)
	}
	if rep.Verify.RoundTripOK != 24 || rep.Verify.RoundTripTotal != 24 {
		t.Errorf("roundtrip=%d/%d want 24/24", rep.Verify.RoundTripOK, rep.Verify.RoundTripTotal)
	}
	if st.KeyframeHits != 24 || st.KeyframeTotal != 24 {
		t.Errorf("keyframe=%d/%d want 24/24", st.KeyframeHits, st.KeyframeTotal)
	}
	if rep.Verify.SHAOK != 24 || rep.Verify.SHATotal != 24 {
		t.Errorf("sha=%d/%d want 24/24", rep.Verify.SHAOK, rep.Verify.SHATotal)
	}
	if st.Representations != 3 {
		t.Errorf("representations=%d want 3", st.Representations)
	}
	if st.Aligned != 8 {
		t.Errorf("aligned=%d want 8", st.Aligned)
	}
	if !rep.Verify.Complete() {
		t.Errorf("verify not complete: %+v", rep.Verify)
	}
}

func TestDamagedQuarantinesTwoAndKeepsHashEvidence(t *testing.T) {
	dir := genTree(t, fixture.DamagedConfig("", 23))
	rep := execDir(t, dir, "damaged", Config{Workers: 2, Queue: 4})

	if rep.ExitCode != 2 {
		t.Fatalf("exit=%d want 2", rep.ExitCode)
	}
	if len(rep.Manifest.Quarantined) != 2 {
		t.Fatalf("quarantined=%d want 2", len(rep.Manifest.Quarantined))
	}
	if rep.Verify.RoundTripOK != 22 || rep.Verify.RoundTripTotal != 22 {
		t.Errorf("roundtrip=%d/%d want 22/22", rep.Verify.RoundTripOK, rep.Verify.RoundTripTotal)
	}
	if rep.Verify.SHAOK != 24 || rep.Verify.SHATotal != 24 {
		t.Errorf("sha=%d/%d want 24/24 (quarantine evidence hashed)", rep.Verify.SHAOK, rep.Verify.SHATotal)
	}
	for _, q := range rep.Manifest.Quarantined {
		if q.Offset < 0 && q.Kind == "" {
			t.Errorf("quarantine record missing fault detail: %+v", q)
		}
	}
}

func TestResultHashIndependentOfScheduling(t *testing.T) {
	dir := genTree(t, fixture.CleanConfig("", 23))
	media := filepath.Join(dir, MediaDir)

	hash := func(workers, queue int) string {
		t.Helper()
		rep, err := Execute(context.Background(), ExecuteParams{
			MediaRoot: media, Config: Config{Workers: workers, Queue: queue},
			PathPrefix: MediaDir, VerifyBaseDir: dir,
			ManifestPath: filepath.Join(dir, "m.json"),
		})
		if err != nil {
			t.Fatalf("Execute w=%d q=%d: %v", workers, queue, err)
		}
		return rep.ResultHash
	}

	h1 := hash(1, 1)
	h4 := hash(4, 8)
	h8 := hash(8, 2)
	if h1 != h4 || h4 != h8 {
		t.Fatalf("result hash varies with scheduling: %s %s %s", h1, h4, h8)
	}
}
