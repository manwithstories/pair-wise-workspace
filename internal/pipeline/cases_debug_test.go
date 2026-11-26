package pipeline

import (
	"path/filepath"
	"testing"

	"mediad/internal/fixtures"
	"mediad/internal/manifest"
	"mediad/internal/media"
)

func runCase(t *testing.T, c string, seed int64) (*Stats, error) {
	t.Helper()
	root := t.TempDir()
	set, err := fixtures.Build(fixtures.Spec{Root: filepath.Join(root, "fx"), Seed: seed, Case: c})
	if err != nil {
		return nil, err
	}
	m, err := manifest.ParseManifestFile(set.Manifest)
	if err != nil {
		return nil, err
	}
	e, err := manifest.ParseEditsFile(set.Edits)
	if err != nil {
		return nil, err
	}
	return Run(Options{MediaRoot: set.Root, Manifest: m, Edits: e, OutDir: filepath.Join(root, "out")})
}

func TestLateWrapCase(t *testing.T) {
	stats, err := runCase(t, fixtures.CaseLateWrap, 41)
	if err != nil {
		t.Fatalf("late-wrap: %v", err)
	}
	if stats.LateEvents == 0 {
		t.Fatal("expected a late event")
	}
	if stats.ReplayedEvents == 0 {
		t.Fatal("expected a replay")
	}
	if stats.DriftPPM == 0 {
		t.Fatal("expected nonzero drift")
	}
	if stats.ManifestAlignUS > 21 {
		t.Fatalf("align=%f", stats.ManifestAlignUS)
	}
}

func TestTruncatedCase(t *testing.T) {
	stats, err := runCase(t, fixtures.CaseTruncated, 41)
	if err != nil {
		t.Fatalf("truncated: %v", err)
	}
	if stats.DamagedRanges == 0 {
		t.Fatal("expected a damaged range")
	}
	if len(stats.Damaged) == 0 || stats.Damaged[0].Camera != "camBroken" {
		t.Fatalf("damage detail: %+v", stats.Damaged)
	}
}

func TestInvalidBaseRejected(t *testing.T) {
	root := t.TempDir()
	set, err := fixtures.Build(fixtures.Spec{Root: root, Seed: 41, Case: fixtures.CaseInvalidTB})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, err = manifest.ParseManifestFile(set.Manifest)
	if err == nil {
		t.Fatal("expected rejection")
	}
	me, ok := media.AsMediaError(err)
	if !ok || me.Code != media.ErrInvalidTimebase {
		t.Fatalf("code=%v err=%v", me, err)
	}
	if media.ExitCode(err) != 2 {
		t.Fatalf("exit=%d", media.ExitCode(err))
	}
}
