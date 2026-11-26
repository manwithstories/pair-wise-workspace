package pipeline_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"pairflow/internal/pipeline"
)

func testConfig(work string) pipeline.Config {
	return pipeline.Config{
		WorkDir:           work,
		Seed:              417,
		Tracks:            2,
		FramesPerTrack:    24,
		PayloadBytes:      512,
		DriftPPM:          120,
		Rollover:          4294967200,
		VFRSeed:           53,
		DropSeqs:          []int{811, 812},
		DamageTrack:       "cam-01",
		DamageFrame:       7,
		CorruptByte:       29,
		CrashAfterRecords: 11,
	}
}

func TestProveColdAndResumeAreByteIdentical(t *testing.T) {
	cfg := testConfig(t.TempDir())
	var out bytes.Buffer
	rep, err := pipeline.Prove(cfg, &out)
	if err != nil {
		t.Fatalf("prove: %v\noutput:\n%s", err, out.String())
	}
	if rep.CleanFrames != 48 {
		t.Fatalf("clean frames=%d want 48", rep.CleanFrames)
	}
	if rep.ColdResumeEqual != 1 || rep.LocationErrorByte != 0 || rep.LocationErrorFrame != 0 {
		t.Fatalf("bad report: %+v", rep)
	}
	if rep.SyncMaxSkewUS > 8000 || rep.ResumeMS > 5000 {
		t.Fatalf("acceptance bounds failed: skew=%dus resume=%dms", rep.SyncMaxSkewUS, rep.ResumeMS)
	}
	cold, err := os.ReadFile(filepath.Join(cfg.WorkDir, "cold", "output.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := os.ReadFile(filepath.Join(cfg.WorkDir, "resumed", "output.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cold, resumed) {
		t.Fatal("cold and resumed output differ")
	}
}

func TestInputChangeInvalidatesCheckpoint(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	ds, err := pipeline.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work")
	if _, err := pipeline.Run(ds, pipeline.Options{Dir: work, CrashAfterRecords: 5}); !errors.Is(err, pipeline.ErrCrash) {
		t.Fatalf("crash err=%v", err)
	}
	cfg.Seed++
	changed, err := pipeline.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := pipeline.Run(changed, pipeline.Options{Dir: work, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.StartRecord != 0 {
		t.Fatalf("start=%d want 0 after input change", res.StartRecord)
	}
}

func TestCorruptMainCheckpointFallsBackToStage(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	ds, err := pipeline.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	crashDir := filepath.Join(dir, "work")
	_, err = pipeline.Run(ds, pipeline.Options{Dir: crashDir, CrashAfterRecords: 11})
	if !errors.Is(err, pipeline.ErrCrash) {
		t.Fatalf("crash err=%v want ErrCrash", err)
	}
	if err := os.WriteFile(filepath.Join(crashDir, "checkpoint.json"), []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	resumed, err := pipeline.Run(ds, pipeline.Options{Dir: crashDir, Resume: true})
	if err != nil {
		t.Fatalf("resume from stage: %v", err)
	}
	if resumed.StartRecord != 11 {
		t.Fatalf("start=%d want 11", resumed.StartRecord)
	}

	coldDir := filepath.Join(dir, "cold")
	cold, err := pipeline.Run(ds, pipeline.Options{Dir: coldDir})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(coldDir, "output.jsonl"))
	b, _ := os.ReadFile(filepath.Join(crashDir, "output.jsonl"))
	if !bytes.Equal(a, b) || cold.OutputSHA256 != resumed.OutputSHA256 {
		t.Fatal("fallback resume output is not identical to cold run")
	}
}
