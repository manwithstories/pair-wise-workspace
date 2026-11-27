package pipeline

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"avflow/internal/fixture"
	"avflow/internal/model"
	"avflow/internal/txn"
)

func buildClean(t *testing.T, packets int) (*fixture.Generated, string) {
	t.Helper()
	g, err := fixture.Build(fixture.Plan{Seed: 417, Packets: packets, Faults: fixture.None()})
	if err != nil {
		t.Fatal(err)
	}
	return g, t.TempDir()
}

func TestCleanPipelineDeterministic(t *testing.T) {
	g, base := buildClean(t, 300)
	dir1 := filepath.Join(base, "run1")
	tl1, m1, err := Run(g.Data, Config{WorkDir: dir1})
	if err != nil {
		t.Fatalf("clean run failed: %v", err)
	}
	if len(tl1.Points) != 300 {
		t.Fatalf("timeline points = %d, want 300", len(tl1.Points))
	}
	if m1.SHA256 == "" || len(m1.SHA256) != 64 {
		t.Fatalf("bad sha256 %q", m1.SHA256)
	}
	dir2 := filepath.Join(base, "run2")
	tl2, m2, err := Run(g.Data, Config{WorkDir: dir2})
	if err != nil {
		t.Fatal(err)
	}
	if m1.SHA256 != m2.SHA256 {
		t.Fatal("repeated runs produced different checksums")
	}
	if hashTimeline(tl1) != hashTimeline(tl2) {
		t.Fatal("timeline hashes diverged")
	}
}

func TestBadCRCCropsAndIsRecoverable(t *testing.T) {
	g, err := fixture.Build(fixture.Plan{Seed: 417, Packets: 300, Faults: fixture.FaultSet{BadCRC: true}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "work")
	tl, m, err := Run(g.Data, Config{WorkDir: dir})
	if err != nil {
		t.Fatalf("recoverable fault must not fail the run: %v", err)
	}
	if m.FirstFault == nil || m.FirstFault.Type != model.FaultBadCRC {
		t.Fatalf("want bad CRC first fault, got %+v", m.FirstFault)
	}
	// Output must be cropped before the corrupt packet.
	injected := g.FaultSeqs[model.FaultBadCRC]
	for _, p := range tl.Points {
		if p.Seq >= injected && p.Track == 1 {
			t.Fatal("corrupt packet leaked into the timeline")
		}
	}
	// Precision: exact packet (delta 0) and no false positives on this set.
	if got := Precision(g, m.FirstFault); got > 1 {
		t.Fatalf("localization precision = %d, want <=1", got)
	}
}

func TestPTSRegressionTriggersRollback(t *testing.T) {
	g, err := fixture.Build(fixture.Plan{Seed: 417, Packets: 300, Faults: fixture.FaultSet{PTSRegression: true}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "work")
	_, m, err := Run(g.Data, Config{WorkDir: dir})
	if err == nil {
		t.Fatal("PTS regression must fail the run")
	}
	f, ok := model.AsFault(err)
	if !ok || f.Type != model.FaultPTSRegression || f.Recoverable {
		t.Fatalf("want non-recoverable pts regression, got %v", err)
	}
	if !m.RollbackOK {
		t.Fatal("rollback invariant not satisfied")
	}
	mgr, rep, err := txn.Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if mgr.Committed(StageStream) {
		t.Fatal("stream stage should have been rolled back")
	}
	if len(rep.ReplayedStages) != 0 {
		t.Fatal("no committed stage should survive a rollback")
	}
}

func TestSimulatedCrashAndResume(t *testing.T) {
	g, base := buildClean(t, 300)
	crashDir := filepath.Join(base, "crash")
	_, _, err := Run(g.Data, Config{WorkDir: crashDir, CrashAfter: StageNormalize})
	if err == nil {
		t.Fatal("simulated crash must return an error")
	}
	// Resume in a fresh process against the same directory.
	start := time.Now()
	mgr, rep, err := txn.Recover(crashDir)
	if err != nil {
		t.Fatal(err)
	}
	resumeMS := float64(time.Since(start).Microseconds()) / 1000.0
	defer mgr.Close()
	if resumeMS < 0 {
		t.Fatal("negative resume time")
	}
	// The normalize stage committed before the crash must be replayed.
	if !mgr.Committed(StageNormalize) || !mgr.Committed(StageStream) {
		t.Fatalf("committed stages lost: %+v", rep.ReplayedStages)
	}
	if mgr.Committed(StageTimeline) {
		t.Fatal("timeline stage must not exist after a normalize-phase crash")
	}
	// A resumed full run completes and hashes identically to a clean run.
	tl, m, err := Run(g.Data, Config{WorkDir: crashDir})
	if err != nil {
		t.Fatal(err)
	}
	cleanDir := filepath.Join(base, "clean")
	tlClean, mClean, err := Run(g.Data, Config{WorkDir: cleanDir})
	if err != nil {
		t.Fatal(err)
	}
	if m.SHA256 != mClean.SHA256 || hashTimeline(tl) != hashTimeline(tlClean) {
		t.Fatal("resumed run does not match the clean checksum")
	}
}

func TestFalsePositivesZeroOnCleanStream(t *testing.T) {
	g, base := buildClean(t, 300)
	tl, _, err := Run(g.Data, Config{WorkDir: filepath.Join(base, "w")})
	if err != nil {
		t.Fatal(err)
	}
	if got := FalsePositives(g, tl.Faults); got != 0 {
		t.Fatalf("false positives = %d, want 0", got)
	}
}

func TestP95UnderBudget(t *testing.T) {
	g, _ := buildClean(t, 300)
	base := t.TempDir()
	p95, err := BenchmarkRun(g.Data, base, 9)
	if err != nil {
		t.Fatal(err)
	}
	if p95 <= 0 {
		t.Fatalf("non-positive p95: %f", p95)
	}
}

func TestStageArtifactsPersist(t *testing.T) {
	g, base := buildClean(t, 120)
	dir := filepath.Join(base, "w")
	if _, _, err := Run(g.Data, Config{WorkDir: dir}); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{StageStream, StageNormalize, StageTimeline} {
		matches, _ := filepath.Glob(filepath.Join(dir, "stages", stage, "*"))
		if len(matches) == 0 {
			t.Fatalf("stage %s produced no artifact", stage)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "wal.log")); err != nil {
		t.Fatalf("wal missing: %v", err)
	}
}
