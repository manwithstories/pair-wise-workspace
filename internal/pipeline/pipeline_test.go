package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"mediabridge/internal/fault"
)

// TestDemoPipelineInterruptResume is the end-to-end contract the CLI demo
// exercises: a run interrupted mid-asset must resume without duplicating
// packets, and the final output must be byte-identical to a clean run.
func TestDemoPipelineInterruptResume(t *testing.T) {
	dir := t.TempDir()
	demo, err := GenerateDemo(DemoOptions{Seed: 41, Loss: 0.03, Reorder: 0.02, RootDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")

	// Interrupted run.
	res1, runErr1 := Run(demo.ManifestPath, Options{OutDir: out, InterruptAfter: 120})
	if !runErr1.Interrupted {
		t.Fatalf("expected interruption, results=%v", statuses(res1))
	}
	interruptedPath := filepath.Join(out, "PT-001.mbx")
	prefix, err := os.ReadFile(interruptedPath)
	if err != nil {
		t.Fatalf("interrupted output missing: %v", err)
	}

	// Resume run.
	res2, runErr2 := Run(demo.ManifestPath, Options{OutDir: out})
	if runErr2.Fatal != nil {
		t.Fatal(runErr2.Fatal)
	}
	var pt1 *AssetResult
	for i := range res2 {
		if res2[i].AssetID == "PT-001" {
			pt1 = &res2[i]
		}
	}
	if pt1 == nil {
		t.Fatal("PT-001 missing from resumed results")
	}
	if pt1.Status != "resumed" {
		t.Fatalf("PT-001 status = %s, want resumed", pt1.Status)
	}
	if pt1.Metrics.ResumedDuplicatePackets != 0 {
		t.Fatalf("duplicate packets = %d", pt1.Metrics.ResumedDuplicatePackets)
	}
	if pt1.Metrics.CopiedPacketHashMatchPct != 100 {
		t.Fatalf("hash match = %f", pt1.Metrics.CopiedPacketHashMatchPct)
	}
	if pt1.Metrics.ProvenanceCoveragePct != 100 {
		t.Fatalf("coverage = %f", pt1.Metrics.ProvenanceCoveragePct)
	}

	// The interrupted prefix must be a prefix of the final output: resume must
	// never rewrite committed bytes.
	final, err := os.ReadFile(interruptedPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(final) < len(prefix) {
		t.Fatalf("final output %d shorter than interrupted prefix %d", len(final), len(prefix))
	}
	if string(final[:len(prefix)]) != string(prefix) {
		t.Fatal("committed prefix was rewritten during resume")
	}

	// Final output must equal a clean, uninterrupted run byte-for-byte.
	cleanDir := filepath.Join(dir, "clean")
	if _, rerr := Run(demo.ManifestPath, Options{OutDir: cleanDir}); rerr.Fatal != nil {
		t.Fatal(rerr.Fatal)
	}
	clean, err := os.ReadFile(filepath.Join(cleanDir, "PT-001.mbx"))
	if err != nil {
		t.Fatal(err)
	}
	if string(clean) != string(final) {
		t.Fatal("resumed output differs from clean run")
	}
}

func TestCorruptAssetIsolated(t *testing.T) {
	dir := t.TempDir()
	demo, err := GenerateDemo(DemoOptions{Seed: 41, RootDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	results, runErr := Run(demo.ManifestPath, Options{OutDir: filepath.Join(dir, "out")})
	if runErr.Fatal != nil {
		t.Fatalf("corrupt asset must not be fatal: %v", runErr.Fatal)
	}
	var accepted, rejected int
	var badErr *fault.Error
	for _, r := range results {
		switch r.Status {
		case "rejected":
			rejected++
			if r.AssetID == "PT-003-BAD" {
				badErr = r.Error
			}
		default:
			accepted++
		}
	}
	if accepted != 2 || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d (%v)", accepted, rejected, statuses(results))
	}
	if badErr == nil {
		t.Fatal("corrupt asset produced no located error")
	}
	if !badErr.Loc.HasOffset || badErr.Loc.SourcePath == "" {
		t.Fatalf("corrupt error lacks location: %+v", badErr.Loc)
	}
}

func TestProvenanceDeterministicAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	demo, err := GenerateDemo(DemoOptions{Seed: 77, RootDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	read := func(out string) string {
		if _, rerr := Run(demo.ManifestPath, Options{OutDir: out}); rerr.Fatal != nil {
			t.Fatal(rerr.Fatal)
		}
		data, err := os.ReadFile(filepath.Join(out, "PT-001.mbx.provenance.json"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	a := read(filepath.Join(dir, "a"))
	b := read(filepath.Join(dir, "b"))
	if a != b {
		t.Fatal("provenance differs across identical runs")
	}
}

func statuses(rs []AssetResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.AssetID + ":" + r.Status
	}
	return out
}
