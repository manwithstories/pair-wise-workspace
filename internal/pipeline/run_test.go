package pipeline

import (
	"errors"
	"testing"

	"github.com/clearliu/mediq/internal/core"
)

func TestRunCleanMetrics(t *testing.T) {
	m, err := Run(Options{Case: "clean", OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if m.Quality.DeviationPct > 0.1 {
		t.Fatalf("deviation=%.4f%% > 0.1", m.Quality.DeviationPct)
	}
	if m.Quality.ManifestErrorMS > 1 {
		t.Fatalf("manifest error=%.3fms > 1", m.Quality.ManifestErrorMS)
	}
	if abs64(m.Quality.IntegratedLUFS+23) > 0.2 {
		t.Fatalf("LUFS=%.3f not within 0.2 of -23", m.Quality.IntegratedLUFS)
	}
	if m.Quality.TruePeakDBTP > -1 {
		t.Fatalf("true peak %.3f exceeds ceiling", m.Quality.TruePeakDBTP)
	}
	if m.Product.SHA256 == "" || m.Counts.MissingSamples != 0 {
		t.Fatalf("bad clean manifest: %+v", m)
	}
}

func TestRunDamagedReportsOffsetAndCounts(t *testing.T) {
	m, err := Run(Options{Case: "damaged", OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if m.FirstParseErrorOffset < 0 {
		t.Fatal("damaged run must report parse offset")
	}
	if m.Counts.ParseErrors != 2 {
		t.Fatalf("parse errors=%d, want 2", m.Counts.ParseErrors)
	}
	if m.Counts.DuplicatePackets != 1 {
		t.Fatalf("duplicates=%d, want 1", m.Counts.DuplicatePackets)
	}
	if m.Counts.LateReplays != 1 || m.Counts.LateExpired != 1 {
		t.Fatalf("replays=%d expired=%d, want 1/1", m.Counts.LateReplays, m.Counts.LateExpired)
	}
	if m.Counts.MissingSamples != 1920 {
		t.Fatalf("missing samples=%d, want 1920", m.Counts.MissingSamples)
	}
}

func TestStrictDamagedExitsTwo(t *testing.T) {
	_, err := Run(Options{Case: "damaged", OutDir: t.TempDir(), Strict: true})
	if core.ExitCode(err) != 2 {
		t.Fatalf("err=%v code=%d, want 2", err, core.ExitCode(err))
	}
}

func TestRecoveryCrashResumeHashesMatchUninterrupted(t *testing.T) {
	interruptedDir := t.TempDir()
	m1, err := Run(Options{Case: "recovery", OutDir: interruptedDir, CrashAfter: "checkpoint"})
	if core.ExitCode(err) != 75 {
		t.Fatalf("crash err=%v code=%d, want 75", err, core.ExitCode(err))
	}
	if m1 == nil || m1.CommittedOutputSamples != 24000 {
		t.Fatalf("partial manifest=%+v", m1)
	}

	resumed, err := Run(Options{Case: "recovery", OutDir: interruptedDir, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	again, err := Run(Options{Case: "recovery", OutDir: interruptedDir, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	normal, err := Run(Options{Case: "recovery", OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Product.SHA256 != again.Product.SHA256 {
		t.Fatal("repeated resume changed product hash")
	}
	if resumed.Product.SHA256 != normal.Product.SHA256 {
		t.Fatalf("resume hash %s != uninterrupted %s", resumed.Product.SHA256, normal.Product.SHA256)
	}
}

func TestResumeWithoutCheckpointIsConfigError(t *testing.T) {
	_, err := Run(Options{Case: "recovery", OutDir: t.TempDir(), Resume: true})
	if code := core.ExitCode(err); code != 1 {
		t.Fatalf("code=%d err=%v, want 1", code, err)
	}
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Kind != core.KindConfig {
		t.Fatalf("error should be classified config: %v", err)
	}
}

func abs64(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
