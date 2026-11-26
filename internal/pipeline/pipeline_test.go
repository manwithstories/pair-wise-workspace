package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/fixtures"
	"mediad/internal/manifest"
)

func buildRun(t *testing.T, root string, c string, seed int64) (*Stats, *fixtures.Set) {
	t.Helper()
	set, err := fixtures.Build(fixtures.Spec{Root: filepath.Join(root, "fx"), Seed: seed, Case: c})
	if err != nil {
		t.Fatalf("fixtures: %v", err)
	}
	m, err := manifest.ParseManifestFile(set.Manifest)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	e, err := manifest.ParseEditsFile(set.Edits)
	if err != nil {
		t.Fatalf("edits: %v", err)
	}
	out := filepath.Join(root, "out")
	stats, err := Run(Options{MediaRoot: set.Root, Manifest: m, Edits: e, OutDir: out})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return stats, set
}

func TestCleanDelivery(t *testing.T) {
	root := t.TempDir()
	stats, _ := buildRun(t, root, fixtures.CaseClean, 41)
	if _, err := os.Stat(stats.OutputPath); err != nil {
		t.Fatalf("output missing: %v", err)
	}
	if stats.ClippedSamples != 0 {
		t.Fatalf("clipped=%d", stats.ClippedSamples)
	}
	if stats.ManifestAlignUS > 21 {
		t.Fatalf("align=%f", stats.ManifestAlignUS)
	}
	if stats.BitrateErrorPct > 8 || stats.BitrateErrorPct < -8 {
		t.Fatalf("bitrate err=%f", stats.BitrateErrorPct)
	}
	if stats.IntegratedLUFS < -17 || stats.IntegratedLUFS > -15 {
		t.Fatalf("lufs=%f", stats.IntegratedLUFS)
	}
}

func TestRepeatHashStable(t *testing.T) {
	root := t.TempDir()
	a, _ := buildRun(t, filepath.Join(root, "a"), fixtures.CaseClean, 41)
	b, _ := buildRun(t, filepath.Join(root, "b"), fixtures.CaseClean, 41)
	if a.OutputSHA256 != b.OutputSHA256 {
		t.Fatalf("hash differs: %s vs %s", a.OutputSHA256, b.OutputSHA256)
	}
}

func TestResumeSkipsVerifiedDelivery(t *testing.T) {
	root := t.TempDir()
	first, _ := buildRun(t, root, fixtures.CaseClean, 41)
	// Resume the same output directory with the same inputs.
	set, err := fixtures.Build(fixtures.Spec{Root: filepath.Join(root, "fx"), Seed: 41, Case: fixtures.CaseClean})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := manifest.ParseManifestFile(set.Manifest)
	e, _ := manifest.ParseEditsFile(set.Edits)
	out := filepath.Join(root, "out")
	second, err := Run(Options{MediaRoot: set.Root, Manifest: m, Edits: e, OutDir: out, Resume: true})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if second.OutputSHA256 != first.OutputSHA256 {
		t.Fatalf("resume hash changed: %s vs %s", first.OutputSHA256, second.OutputSHA256)
	}
}

func TestFailedRunLeavesNoDelivery(t *testing.T) {
	root := t.TempDir()
	set, err := fixtures.Build(fixtures.Spec{Root: filepath.Join(root, "fx"), Seed: 41, Case: fixtures.CaseTruncated})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := manifest.ParseManifestFile(set.Manifest)
	e, _ := manifest.ParseEditsFile(set.Edits)
	// Force a failure by pointing at a missing output parent that cannot be
	// created (a regular file), so the encoder cannot open its temp file.
	badOut := filepath.Join(root, "blob")
	if err := os.WriteFile(badOut, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Run(Options{MediaRoot: set.Root, Manifest: m, Edits: e, OutDir: filepath.Join(badOut, "out")})
	if err == nil {
		t.Fatal("expected failure")
	}
	if _, statErr := os.Stat(filepath.Join(badOut, "out", "delivery.m4a")); statErr == nil {
		t.Fatal("failed run left a delivery file")
	}
}
