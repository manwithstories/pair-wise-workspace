package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediarun/internal/testkit"
)

func TestRunFullPipeline(t *testing.T) {
	dir := t.TempDir()
	res, err := Run(Options{
		Seed:      42,
		Scenario:  testkit.ScenarioBurst,
		Bandwidth: []int{2, 8, 1},
		Out:       dir,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.RoundTripRate != 1 {
		t.Fatalf("roundtrip %f", res.RoundTripRate)
	}
	if res.KeyframeRate != 1 {
		t.Fatalf("keyframe %f", res.KeyframeRate)
	}
	if res.Segments < 3 || len(res.SegmentHashes) != res.Segments {
		t.Fatalf("segments %d hashes %d", res.Segments, len(res.SegmentHashes))
	}
	if res.DetectedGaps <= 0 {
		t.Fatal("burst scenario produced no gaps")
	}
	if res.RollbackCount <= 0 || res.Recovered != 1 || res.CommittedMatch != 1 {
		t.Fatalf("recovery counters: %+v", res)
	}
	for _, h := range res.SegmentHashes {
		if len(h) != 64 {
			t.Fatalf("hash %q", h)
		}
	}
	// Artifacts exist inside the output directory.
	for _, name := range []string{"manifest.json", "wal.jsonl", "manifest.jsonl", "transport.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "frames")); err != nil {
		t.Fatalf("frames: %v", err)
	}
}

func TestRunReproducibleHashes(t *testing.T) {
	base, err := Run(Options{Seed: 5, Scenario: testkit.ScenarioReorder, Bandwidth: []int{4}, Out: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	again, err := Run(Options{Seed: 5, Scenario: testkit.ScenarioReorder, Bandwidth: []int{4}, Out: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(base.SegmentHashes, ",") != strings.Join(again.SegmentHashes, ",") {
		t.Fatalf("hashes differ:\n%v\n%v", base.SegmentHashes, again.SegmentHashes)
	}
}

func TestRunRejectsBadOutput(t *testing.T) {
	if _, err := Run(Options{Seed: 1, Scenario: testkit.ScenarioClean, Out: "/dev/null/nope"}); err == nil {
		t.Fatal("bad output dir accepted")
	}
}
