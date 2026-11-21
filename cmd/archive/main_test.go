package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"mkvarchive/internal/fixture"
	"mkvarchive/internal/pipeline"
)

func TestDemoStandardWritesArtifacts(t *testing.T) {
	dir := t.TempDir()
	sample := fixture.Generate(fixture.CaseStandard, fixture.DemoClusters)
	if err := os.WriteFile(filepath.Join(dir, "sample.mkv"), sample.Data, 0o644); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(dir, "archive.mkv")
	af, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := pipeline.Archive(bytes.NewReader(sample.Data), af, dir, pipeline.DefaultOptions())
	af.Close()
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if res.Outcome.ExitCode() != 0 {
		t.Fatalf("standard run must exit 0, got %d", res.Outcome.ExitCode())
	}
	// The pipeline itself writes the archive and checkpoint; manifest, plan
	// and evidence files are the CLI layer's responsibility.
	for _, name := range []string{"archive.mkv", "checkpoint.json"} {
		if _, err = os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("artifact %s missing: %v", name, err)
		}
	}
	if res.Manifest == nil || len(res.PlanJSON) == 0 {
		t.Fatal("pipeline must still produce the manifest and plan in memory")
	}
}

func TestRecoverTruncatedExitCodeTwo(t *testing.T) {
	dir := t.TempDir()
	sample := fixture.Generate(fixture.CaseTruncated, fixture.DemoClusters)
	af, err := os.Create(filepath.Join(dir, "recovered.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := pipeline.Archive(bytes.NewReader(sample.Data), af, dir, pipeline.DefaultOptions())
	af.Close()
	if err != nil {
		t.Fatalf("recover should tolerate damage: %v", err)
	}
	if res.Outcome != pipeline.OutcomePartial || res.Outcome.ExitCode() != 2 {
		t.Fatalf("want partial/2, got %v/%d", res.Outcome, res.Outcome.ExitCode())
	}
	if res.FirstErrorOffset <= 0 || res.FirstErrorCode != "EBML_TRUNCATED_HEADER" {
		t.Fatalf("recovery evidence wrong: %+v", res)
	}
	if res.RecoveredBlocks < 1 || res.RecoveredBlocks >= res.TotalSourceBlocks {
		t.Fatalf("recovered=%d total=%d", res.RecoveredBlocks, res.TotalSourceBlocks)
	}
}

func TestCLIDemoWritesAllArtifacts(t *testing.T) {
	dir := t.TempDir()
	if err := runDemo([]string{"--out", dir}); err != nil {
		t.Fatalf("runDemo: %v", err)
	}
	for _, name := range []string{"sample.mkv", "archive.mkv", "manifest.json",
		"plan.json", "quality.json", "checkpoint.json", "evidence.mp4"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("artifact %s missing: %v", name, err)
		}
	}
}

func TestCLIRecoverTruncatedReportsPartial(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	sample := fixture.Generate(fixture.CaseTruncated, 8)
	sourcePath := filepath.Join(srcDir, "sample.mkv")
	if err := os.WriteFile(sourcePath, sample.Data, 0o644); err != nil {
		t.Fatal(err)
	}
	err := runRecover([]string{"--input", sourcePath, "--out", outDir})
	if ce, ok := err.(*cliError); !ok || ce.code != 2 {
		t.Fatalf("want cliError code 2, got %v", err)
	}
	if _, err = os.Stat(filepath.Join(outDir, "recovered.mkv")); err != nil {
		t.Fatalf("recovered output missing: %v", err)
	}
}

func TestAllDamageCasesStayRunnable(t *testing.T) {
	for _, c := range []fixture.Case{
		fixture.CaseUnknownBlock, fixture.CaseChannelMismatch,
		fixture.CaseSampleRateConflict, fixture.CaseColorMissing,
	} {
		dir := t.TempDir()
		sample := fixture.Generate(c, 12)
		var dst bytes.Buffer
		res, err := pipeline.Archive(bytes.NewReader(sample.Data), &dst, dir,
			pipeline.DefaultOptions())
		if err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		if dst.Len() == 0 {
			t.Fatalf("%s: empty archive output", c)
		}
		if res.Outcome != pipeline.OutcomeSuccess {
			t.Fatalf("%s: metadata conflicts must not hard-fail under retain, got %v",
				c, res.Outcome)
		}
	}
}
