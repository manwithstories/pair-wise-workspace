package demo

import (
	"os"
	"path/filepath"
	"testing"

	"mediacore/internal/fixture"
	"mediacore/internal/pipeline"
	"mediacore/internal/provenance"
	"mediacore/internal/store"
)

func setup(t *testing.T) (Params, *fixture.Result, *pipeline.Context, string) {
	t.Helper()
	dir := t.TempDir()
	fixtureDir := filepath.Join(dir, "fixtures")
	fix, err := fixture.Generate(fixtureDir, 42)
	if err != nil {
		t.Fatal(err)
	}
	params := DefaultParams(42, fixtureDir)
	st, err := store.Open(filepath.Join(dir, "checkpoints"))
	if err != nil {
		t.Fatal(err)
	}
	inputDigest, err := InputDigest(fixtureDir, fix)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &pipeline.Context{
		Store:        st,
		Prov:         provenance.Open(filepath.Join(dir, "prov.jsonl")),
		InputDigest:  inputDigest,
		ParamsDigest: ParamsDigest(params),
	}
	return params, fix, ctx, filepath.Join(dir, "checkpoints")
}

func runOnce(t *testing.T, params Params, fix *fixture.Result, ctx *pipeline.Context) (*pipeline.Summary, Summary) {
	t.Helper()
	r, err := pipeline.New(Nodes(params, fix))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := r.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rep := ctx.Get("report").(*ReportResult)
	return sum, rep.Summary
}

func TestEndToEndIdempotent(t *testing.T) {
	params, fix, ctx, _ := setup(t)
	sum1, s1 := runOnce(t, params, fix, ctx)
	if sum1.Commits != 10 || sum1.RecoveredNodes != 0 {
		t.Fatalf("first run: %+v", sum1)
	}
	if s1.DeviationPct > 5.0 {
		t.Fatalf("bitrate deviation %v exceeds 5%%", s1.DeviationPct)
	}
	if s1.ManifestAlignmentMs > 40 {
		t.Fatalf("alignment %v exceeds 40ms", s1.ManifestAlignmentMs)
	}
	if s1.SilenceSegments != 2 {
		t.Fatalf("silence = %d, want 2", s1.SilenceSegments)
	}
	if s1.DuplicateSegments == 0 || s1.QualitySamples != 14 {
		t.Fatalf("bad summary: %+v", s1)
	}

	// Second run over the same state: identical summary, no commits.
	params2, _, ctx2 := params, fix, ctx
	sum2, s2 := runOnce(t, params2, fix, ctx2)
	if sum2.Commits != 0 || sum2.DuplicateCommits != 0 {
		t.Fatalf("second run must not commit: %+v", sum2)
	}
	if s1 != s2 {
		t.Fatalf("summaries differ:\n%+v\n%+v", s1, s2)
	}
}

func TestEndToEndCorruptRecovery(t *testing.T) {
	params, fix, ctx, cpDir := setup(t)
	if _, s1 := runOnce(t, params, fix, ctx); s1.SilenceSegments != 2 {
		t.Fatal("setup run failed")
	}
	// Corrupt the cut checkpoint and re-run: cut and report roll back.
	p := filepath.Join(cpDir, "cut.json")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xFF
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum, s2 := runOnce(t, params, fix, ctx)
	if sum.RecoveredNodes != 2 {
		t.Fatalf("recovered = %d, want 2 (cut, report)", sum.RecoveredNodes)
	}
	if sum.FirstMissingDependency != "cut" {
		t.Fatalf("first missing = %q", sum.FirstMissingDependency)
	}
	if s2.SilenceSegments != 2 || s2.QualitySamples != 14 {
		t.Fatalf("recovered summary wrong: %+v", s2)
	}
}
