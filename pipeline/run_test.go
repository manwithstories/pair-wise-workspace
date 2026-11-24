package pipeline_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bcast/pipeline"
)

// runBatch runs the demo graph into a temp dir and returns the summary.
func runBatch(t *testing.T, p pipeline.Params) (*pipeline.Summary, *pipeline.Checkpoint) {
	t.Helper()
	dir := t.TempDir()
	p.OutDir = filepath.Join(dir, "out")
	cpPath := filepath.Join(dir, "cp.json")
	cp, err := pipeline.LoadCheckpoint(cpPath)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := pipeline.Run(context.Background(), p, cp)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return sum, cp
}

func demoParams() pipeline.Params {
	p := pipeline.DefaultParams()
	p.Damage, _ = pipeline.ParseDamage("truncate,broken-box,missing-index")
	return p
}

func TestRunMeetsAcceptanceThresholds(t *testing.T) {
	sum, _ := runBatch(t, demoParams())
	if sum.RoundtripMatchRate < 0.98 {
		t.Fatalf("roundtrip=%v", sum.RoundtripMatchRate)
	}
	if sum.KeyframeHitRate < 0.90 {
		t.Fatalf("keyframe hit=%v", sum.KeyframeHitRate)
	}
	if sum.SegmentHashes != 12 {
		t.Fatalf("segment hashes=%d", sum.SegmentHashes)
	}
	if sum.DamagedRanges < 2 {
		t.Fatalf("damaged ranges=%d", sum.DamagedRanges)
	}
	if sum.BlockedDependency == "" {
		t.Fatal("no blocking dependency reported")
	}
	if sum.RecoveredNodes < 1 {
		t.Fatalf("recovered nodes=%d", sum.RecoveredNodes)
	}
	if !sum.PrimaryOK {
		t.Fatal("primary delivery should be healthy")
	}
	if sum.DuplicateGroups != 1 {
		t.Fatalf("duplicate groups=%d (expected the fixture's copy pair)", sum.DuplicateGroups)
	}
}

func TestRunIsDeterministicAcrossRuns(t *testing.T) {
	a, _ := runBatch(t, demoParams())
	b, _ := runBatch(t, demoParams())
	if a.HashDigest != b.HashDigest {
		t.Fatalf("segment hash digest drifted: %s != %s", a.HashDigest, b.HashDigest)
	}
	if a.Encode() != b.Encode() {
		t.Fatalf("summary differs:\n%s\n%s", a.Encode(), b.Encode())
	}
}

func TestResumeProducesSameResult(t *testing.T) {
	dir := t.TempDir()
	p := demoParams()
	p.OutDir = filepath.Join(dir, "out")
	cpPath := filepath.Join(dir, "cp.json")

	cp1, _ := pipeline.LoadCheckpoint(cpPath)
	first, err := pipeline.Run(context.Background(), p, cp1)
	if err != nil {
		t.Fatal(err)
	}
	// Delete one output to prove verify re-reads from disk rather than memory.
	os.Remove(filepath.Join(p.OutDir, "clean", "seg-000.mp4"))
	// Re-run against the same checkpoint: every completed node is reused.
	cp2, err := pipeline.LoadCheckpoint(cpPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pipeline.Run(context.Background(), p, cp2)
	if err != nil {
		t.Fatal(err)
	}
	if first.HashDigest != second.HashDigest {
		t.Fatalf("resume changed hash digest: %s != %s", first.HashDigest, second.HashDigest)
	}
	if second.ResumedNodes == 0 {
		t.Fatal("resume reused no nodes")
	}
}

func TestCorruptInputBlocksOnlyItself(t *testing.T) {
	sum, cp := runBatch(t, demoParams())
	if !sum.PrimaryOK {
		t.Fatal("a quarantined corrupt input must not fail the primary")
	}
	if sum.Quarantined != 1 {
		t.Fatalf("quarantined=%d", sum.Quarantined)
	}
	st := cp.Nodes["parse:corrupt"]
	if st.State != pipeline.StateFailed {
		t.Fatalf("corrupt parse state=%s", st.State)
	}
	// Downstream of the corrupt input names the root dependency.
	if dep := cp.Nodes["remux:corrupt"].Dep; dep != "parse:corrupt" {
		t.Fatalf("blocked remux dep=%q", dep)
	}
}

func TestVerifyAfterRun(t *testing.T) {
	dir := t.TempDir()
	p := demoParams()
	p.OutDir = filepath.Join(dir, "out")
	cpPath := filepath.Join(dir, "cp.json")
	cp, _ := pipeline.LoadCheckpoint(cpPath)
	if _, err := pipeline.Run(context.Background(), p, cp); err != nil {
		t.Fatal(err)
	}
	reloaded, err := pipeline.LoadCheckpoint(cpPath)
	if err != nil {
		t.Fatal(err)
	}
	v, err := pipeline.Verify(reloaded)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v.Files != 12 || v.SegmentHashes != 12 {
		t.Fatalf("verify files=%d hashes=%d", v.Files, v.SegmentHashes)
	}
	if v.MatchRate < 0.98 {
		t.Fatalf("verify match rate=%v", v.MatchRate)
	}
}

func TestVerifyDetectsTamperedOutput(t *testing.T) {
	dir := t.TempDir()
	p := demoParams()
	p.OutDir = filepath.Join(dir, "out")
	cpPath := filepath.Join(dir, "cp.json")
	cp, _ := pipeline.LoadCheckpoint(cpPath)
	if _, err := pipeline.Run(context.Background(), p, cp); err != nil {
		t.Fatal(err)
	}
	// Corrupt a delivered file in place; verify must reject it.
	target := filepath.Join(p.OutDir, "clean", "seg-001.mp4")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := pipeline.LoadCheckpoint(cpPath)
	if _, err := pipeline.Verify(reloaded); err == nil {
		t.Fatal("verify accepted a tampered output")
	}
}

func TestUnknownDamageRejected(t *testing.T) {
	if _, err := pipeline.ParseDamage("truncate,bogus"); err == nil {
		t.Fatal("unknown damage accepted")
	}
}

func TestDamagedManifestWritten(t *testing.T) {
	dir := t.TempDir()
	p := demoParams()
	p.OutDir = filepath.Join(dir, "out")
	cp, _ := pipeline.LoadCheckpoint(filepath.Join(dir, "cp.json"))
	sum, err := pipeline.Run(context.Background(), p, cp)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(p.OutDir, "damaged-ranges.txt"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "truncate") || !strings.Contains(text, "broken-box") {
		t.Fatalf("manifest missing deliveries:\n%s", text)
	}
	// The deliberately corrupt input is missing moov, not a broken box.
	if !strings.Contains(text, "missing moov") {
		t.Fatalf("corrupt input misclassified:\n%s", text)
	}
	if len(sum.DamagedList) < 2 {
		t.Fatalf("damaged list=%v", sum.DamagedList)
	}
}
