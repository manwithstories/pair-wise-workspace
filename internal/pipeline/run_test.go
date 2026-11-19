package pipeline_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"archivearchivist/internal/model"
	"archivearchivist/internal/pipeline"
	"archivearchivist/internal/testkit"
)

func runFixture(t *testing.T) (*pipeline.Analysis, string) {
	t.Helper()
	dir := t.TempDir()
	fx, err := testkit.Generate(testkit.Config{OutDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	a, err := pipeline.Run(pipeline.Input{
		TimelinePath: fx.Timeline,
		CapturePath:  fx.Capture,
	}, pipeline.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return a, filepath.Dir(fx.Timeline)
}

func TestPipelineMetricsOnCleanFixture(t *testing.T) {
	a, _ := runFixture(t)
	m := a.Metrics
	if m.Tracks != 4 {
		t.Fatalf("tracks = %d, want 4", m.Tracks)
	}
	if m.CorruptRanges < 1 {
		t.Fatalf("corrupt ranges = %d, want >= 1", m.CorruptRanges)
	}
	if m.RecoveredRanges != m.CorruptRanges-m.UnrecoverableRanges {
		t.Fatalf("range accounting inconsistent: %+v", m)
	}
	if m.Wraps < 1 {
		t.Fatalf("wraps = %d, want >= 1", m.Wraps)
	}
	if m.ConstantFPS {
		t.Fatalf("constant_fps = true, want false (proxy track is VFR)")
	}
	if m.PacketRecoveryRate.Cmp(model.NewRat(9, 10)) < 0 {
		t.Fatalf("packet recovery = %s, want >= 0.90", m.PacketRecoveryRate)
	}
	if m.SyncErrorP95US > 20_000 {
		t.Fatalf("sync p95 = %d us, want <= 20000", m.SyncErrorP95US)
	}
	if m.RecoveredBoundaries == 0 {
		t.Fatalf("no boundaries recovered")
	}
	if !a.Plan.Recovered {
		t.Fatalf("plan not recovered: %v", a.Plan.Faults)
	}
}

func TestPipelineIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	fx, err := testkit.Generate(testkit.Config{OutDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	in := pipeline.Input{TimelinePath: fx.Timeline, CapturePath: fx.Capture}
	first, err := pipeline.Run(in, pipeline.Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := pipeline.Run(in, pipeline.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Latency is wall-clock and legitimately varies; every other field is
	// deterministic for the same input.
	a, b := first.Metrics, second.Metrics
	a.LatencyP95US, a.LatencyMeanUS = 0, 0
	b.LatencyP95US, b.LatencyMeanUS = 0, 0
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(ab) != string(bb) {
		t.Fatalf("metrics differ between runs:\n%s\n%s", ab, bb)
	}
}

func TestCycleFixtureBlocksOnlyCycleBranch(t *testing.T) {
	dir := t.TempDir()
	fx, err := testkit.Generate(testkit.Config{OutDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	tl, err := model.LoadTimeline(fx.Timeline)
	if err != nil {
		t.Fatal(err)
	}
	// Introduce a cycle between A1 and A2 while V1 and P1 stay independent.
	for i := range tl.Tracks {
		switch tl.Tracks[i].ID {
		case "A1":
			tl.Tracks[i].DependsOn = []string{"V1", "A2"}
		case "A2":
			tl.Tracks[i].DependsOn = []string{"A1"}
		}
	}
	cycPath := filepath.Join(dir, "timeline-cycle.json")
	if err := writeTimelineJSON(cycPath, tl); err != nil {
		t.Fatal(err)
	}
	a, err := pipeline.Run(pipeline.Input{TimelinePath: cycPath, CapturePath: fx.Capture}, pipeline.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !a.Faults.Has(model.CodeDependencyCycle) {
		t.Fatalf("cycle not reported: %v", a.Faults)
	}
	if a.Metrics.BlockedBoundaries == 0 {
		t.Fatalf("cycle did not block any boundary")
	}
	if a.Metrics.RecoveredBoundaries == 0 {
		t.Fatalf("independent branches were wrongly blocked")
	}
}

func writeTimelineJSON(path string, tl *model.Timeline) error {
	b, err := json.MarshalIndent(tl, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func TestMissingSourceIsReportedNotFatal(t *testing.T) {
	dir := t.TempDir()
	fx, err := testkit.Generate(testkit.Config{OutDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	tl, err := model.LoadTimeline(fx.Timeline)
	if err != nil {
		t.Fatal(err)
	}
	// Point one track at a file that does not exist.
	for i := range tl.Tracks {
		if tl.Tracks[i].ID == "P1" {
			tl.Tracks[i].Source = "absent.mp4"
		}
	}
	b, _ := json.MarshalIndent(tl, "", "  ")
	path := filepath.Join(dir, "missing-source.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := pipeline.Run(pipeline.Input{TimelinePath: path, CapturePath: fx.Capture}, pipeline.Options{})
	if err != nil {
		t.Fatalf("a missing source must not abort the run: %v", err)
	}
	if !a.Faults.Has(model.CodeInvalidInput) {
		t.Fatalf("missing source not reported: %v", a.Faults)
	}
	for _, st := range a.Plan.Tracks {
		if st.Track == "P1" && st.OK {
			t.Fatalf("missing source reported usable")
		}
	}
}

func TestMalformedTimelineIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Run(pipeline.Input{TimelinePath: path}, pipeline.Options{}); err == nil {
		t.Fatalf("malformed timeline accepted")
	}
}
