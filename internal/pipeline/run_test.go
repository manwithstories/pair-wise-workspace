package pipeline

import (
	"errors"
	"path/filepath"
	"testing"

	"mediad/internal/fixture"
	"mediad/internal/model"
)

func prepareFixture(t *testing.T) (inDir, outDir string) {
	t.Helper()
	inDir = t.TempDir()
	outDir = t.TempDir()
	if err := fixture.Generate(inDir); err != nil {
		t.Fatal(err)
	}
	return inDir, outDir
}

func TestDeliverThenResumeProducesStableDigest(t *testing.T) {
	inDir, outDir := prepareFixture(t)
	profile := filepath.Join(inDir, fixture.LossProfileName)

	// interrupted run after 8 samples -> exit condition 75
	interrupted, err := Run(Config{
		InputDir: inDir, OutDir: outDir, FaultProfile: profile,
		StopAfterSamples: 8,
	})
	if !errors.Is(err, model.ErrInterrupted) {
		t.Fatalf("want ErrInterrupted, got %v", err)
	}
	if !interrupted.Interrupted {
		t.Fatal("result must flag interruption")
	}
	cpStore, _ := NewCheckpointStore(outDir)
	cp, err := cpStore.Load()
	if err != nil || cp == nil || !cp.Interrupted {
		t.Fatalf("checkpoint must be persisted and marked interrupted: %v", err)
	}

	// resume completes the deliverable
	resumed, err := Run(Config{
		InputDir: inDir, OutDir: outDir, FaultProfile: profile, Resume: true,
	})
	if err != nil {
		t.Fatalf("resume must succeed: %v", err)
	}
	if resumed.Manifest.RecoveredPackets < 1 {
		t.Fatalf("recovered_packets = %d, want >= 1", resumed.Manifest.RecoveredPackets)
	}
	if resumed.Manifest.UnrecoveredPackets <= 0 {
		t.Fatal("profile guarantees an unrecoverable burst")
	}
	firstDigest := resumed.Manifest.OutputSHA256

	// a clean full run on a fresh output must yield the identical digest
	outDir2 := t.TempDir()
	clean, err := Run(Config{InputDir: inDir, OutDir: outDir2, FaultProfile: profile})
	if err != nil {
		t.Fatal(err)
	}
	if clean.Manifest.OutputSHA256 != firstDigest {
		t.Fatalf("output digest not stable: resume=%s clean=%s",
			firstDigest, clean.Manifest.OutputSHA256)
	}
}

func TestCheckpointDigestMismatchRestartsClean(t *testing.T) {
	inDir, _ := prepareFixture(t)
	outDir := t.TempDir()
	profile := filepath.Join(inDir, fixture.LossProfileName)
	_, err := Run(Config{InputDir: inDir, OutDir: outDir,
		FaultProfile: profile, StopAfterSamples: 4})
	if !errors.Is(err, model.ErrInterrupted) {
		t.Fatal(err)
	}
	// corrupt the checkpoint's input digest so validation must reject it
	cpStore, _ := NewCheckpointStore(outDir)
	cp, _ := cpStore.Load()
	cp.InputSHA256 = "00" + cp.InputSHA256[2:]
	if err := cpStore.Save(cp); err != nil {
		t.Fatal(err)
	}
	res, err := Run(Config{InputDir: inDir, OutDir: outDir,
		FaultProfile: profile, Resume: true})
	if err != nil {
		t.Fatalf("stale checkpoint must trigger clean restart, not error: %v", err)
	}
	if res.Manifest.OutputSHA256 == "" {
		t.Fatal("clean restart must still produce a deliverable")
	}
}

func TestRepeatedRunsAreByteIdentical(t *testing.T) {
	inDir, _ := prepareFixture(t)
	profile := filepath.Join(inDir, fixture.LossProfileName)
	a, err := Run(Config{InputDir: inDir, OutDir: t.TempDir(), FaultProfile: profile})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(Config{InputDir: inDir, OutDir: t.TempDir(), FaultProfile: profile})
	if err != nil {
		t.Fatal(err)
	}
	if a.Manifest.OutputSHA256 != b.Manifest.OutputSHA256 {
		t.Fatal("identical inputs/profile must produce identical output digests")
	}
}
