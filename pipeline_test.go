package pairflow_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"pairflow/internal/pipeline"
)

func TestPipelineFileContract(t *testing.T) {
	work := t.TempDir()
	cfg := pipeline.Config{
		WorkDir: work, Seed: 9, Tracks: 2, FramesPerTrack: 12, PayloadBytes: 256,
		DriftPPM: 40, Rollover: 4294967290, VFRSeed: 3, DropSeqs: []int{811, 812},
		DamageTrack: "cam-02", DamageFrame: 4, CorruptByte: 29, CrashAfterRecords: 7,
	}
	rep, err := pipeline.Prove(cfg, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.ColdResumeEqual != 1 || rep.CleanFrames != 24 {
		t.Fatalf("report=%+v", rep)
	}
	a, err := os.ReadFile(filepath.Join(work, "cold", "output.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(work, "resumed", "output.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("outputs differ")
	}
}
