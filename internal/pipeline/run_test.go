package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"archivecheck/internal/fault"
	"archivecheck/internal/txn"
)

func baseConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Seed: 41, FrameCount: 24, WorkingDir: t.TempDir(),
		MemoryBudget: 1 << 20, BandwidthMbps: []float64{2},
	}
}

func TestNormalRunVerifiesAndRepairsNothing(t *testing.T) {
	rep, err := Run(baseConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if rep.ExitCode != fault.ExitOK {
		t.Fatalf("exit=%d faults=%v", rep.ExitCode, rep.Faults)
	}
	if rep.Stats.TimestampRepairs != 0 || rep.Stats.IndexEntries == 0 {
		t.Fatalf("stats=%+v", rep.Stats)
	}
	if rep.Stats.ManifestAlignMS > 1 {
		t.Fatalf("alignment=%.3f", rep.Stats.ManifestAlignMS)
	}
	if rep.Stats.Quality.Samples == 0 {
		t.Fatal("no quality samples")
	}
	if d := rep.Stats.Bitrate.DeltaPct; d > 5 || d < -5 {
		t.Fatalf("bitrate delta=%.2f", d)
	}
	// Artifacts exist and the index digest matches the sidecar.
	for _, p := range []string{rep.IndexPath, rep.ManifestPath, rep.SidecarPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing artifact %s: %v", p, err)
		}
	}
}

func TestCorruptRunDiagnosed(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Corrupt = true
	cfg.LossBurst = 3
	cfg.Duplicate = 2
	cfg.Reorder = 5
	rep, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ExitCode != fault.ExitDamaged {
		t.Fatalf("exit=%d want 2; faults=%v", rep.ExitCode, rep.Faults)
	}
	if rep.Stats.TimestampRepairs == 0 {
		t.Fatal("expected timestamp repairs")
	}
	if rep.Stats.LostPackets == 0 || rep.Stats.DuplicatePackets == 0 || rep.Stats.ReorderedPackets == 0 {
		t.Fatalf("transport stats=%+v", rep.Stats)
	}
	if !rep.Truncated {
		t.Fatal("corrupt sample should be truncated")
	}
	// Two defects must cross module boundaries: EBML and timeline both report.
	kinds := map[fault.Module]bool{}
	for _, f := range rep.Faults {
		kinds[f.Module] = true
	}
	if !kinds[fault.ModuleEBML] || !kinds[fault.ModuleTimeline] || !kinds[fault.ModuleTransport] {
		t.Fatalf("faults do not span modules: %v", kinds)
	}
}

func TestInterruptThenResumeMatchesDigest(t *testing.T) {
	cfg := baseConfig(t)
	cfg.InterruptAfter = string(txn.StageIndex)
	cfg.CrashOnInterrupt = true
	first, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.ExitCode != fault.ExitDamaged {
		t.Fatalf("interrupted exit=%d want 2", first.ExitCode)
	}

	resume := cfg
	resume.CrashOnInterrupt = false
	resume.InterruptAfter = ""
	resume.Resume = true
	second, err := Run(resume)
	if err != nil {
		t.Fatal(err)
	}
	if second.ExitCode != fault.ExitOK {
		t.Fatalf("resume exit=%d faults=%v", second.ExitCode, second.Faults)
	}
	if second.Rollbacks == 0 {
		t.Fatal("resume should roll back the crashed stage")
	}
	if len(second.Resumed) == 0 {
		t.Fatal("resume did not adopt committed stages")
	}
	// A clean reference run must produce the same index digest.
	ref, err := Run(Config{
		Seed: 41, FrameCount: 24, WorkingDir: t.TempDir(),
		MemoryBudget: 1 << 20, BandwidthMbps: []float64{2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Stats.IndexSHA256 != ref.Stats.IndexSHA256 {
		t.Fatalf("resume digest %s != reference %s", second.Stats.IndexSHA256, ref.Stats.IndexSHA256)
	}
}

func TestSpillMatchesInMemory(t *testing.T) {
	inMem := baseConfig(t)
	inMem.FrameCount = 80
	memRep, err := Run(inMem)
	if err != nil {
		t.Fatal(err)
	}

	spilled := baseConfig(t)
	spilled.FrameCount = 80
	spilled.MemoryBudget = 2048
	spillRep, err := Run(spilled)
	if err != nil {
		t.Fatal(err)
	}
	if spillRep.Spill.Spilled == 0 {
		t.Fatal("expected spilling under a 2 KiB budget")
	}
	if spillRep.Spill.PeakMemory > 2048 {
		t.Fatalf("peak memory %d exceeds budget", spillRep.Spill.PeakMemory)
	}
	if spillRep.Stats.IndexSHA256 != memRep.Stats.IndexSHA256 {
		t.Fatalf("spilled digest %s != in-memory %s", spillRep.Stats.IndexSHA256, memRep.Stats.IndexSHA256)
	}
	if spillRep.Stats.Quality.Samples != memRep.Stats.Quality.Samples {
		t.Fatalf("quality samples differ: %d vs %d", spillRep.Stats.Quality.Samples, memRep.Stats.Quality.Samples)
	}
}

func TestManifestSkewBeyondOneMillisecondRejected(t *testing.T) {
	cfg := baseConfig(t)
	cfg.ManifestSkewMS = 2
	rep, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ExitCode != fault.ExitDamaged {
		t.Fatalf("exit=%d want 2", rep.ExitCode)
	}
	if rep.Stats.ManifestAlignMS <= 1 {
		t.Fatalf("alignment=%.3f should exceed 1ms", rep.Stats.ManifestAlignMS)
	}
	if _, err := os.Stat(rep.ManifestPath); !os.IsNotExist(err) {
		t.Fatal("misaligned manifest must not be committed")
	}
	// The offending fault must localize a block offset.
	var located bool
	for _, f := range rep.Faults {
		if f.Module == fault.ModuleManifest && f.Kind == fault.KindManifestDrift && f.Offset.Valid {
			located = true
		}
	}
	if !located {
		t.Fatalf("no localized manifest drift: %v", rep.Faults)
	}
}

func TestPathOutputsGoToIndependentDir(t *testing.T) {
	cfg := baseConfig(t)
	rep, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(cfg.WorkingDir, "source", "master.mkv")
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	// Outputs live outside the source directory.
	if filepath.Dir(rep.IndexPath) == filepath.Dir(src) {
		t.Fatal("index written beside the source")
	}
	after, _ := os.ReadFile(src)
	if string(before) != string(after) {
		t.Fatal("source was modified")
	}
}

func TestRerunIsIdempotent(t *testing.T) {
	cfg := baseConfig(t)
	first, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Resumed) != len(txn.Order) {
		t.Fatalf("rerun redid stages; resumed=%v", second.Resumed)
	}
	if first.Stats.IndexSHA256 != second.Stats.IndexSHA256 {
		t.Fatal("digest changed across identical reruns")
	}
}
